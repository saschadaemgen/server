// Package doorbellcalls owns the per-call lifecycle row in the
// doorbell_calls table (Migration 010). It is the race-free
// arbiter that decides which viewer "wins" the answer when more
// than one phone, tablet, browser tab or ESP display is ringing
// for the same household.
//
// Wire-up:
//
//	doorbellhub on doorbell_start          -> Start(event_id, mock_mac, device_id)
//	doorbellhub on doorbell_cancel         -> MarkEnded(event_id, "", "timeout")
//	web-viewer POST /webviewer/answer      -> MarkAnswered(event_id, viewer_mac)
//	web-viewer POST /webviewer/reject      -> MarkRejected(event_id, viewer_mac)
//	web-viewer POST /webviewer/end-call    -> MarkEnded(event_id, viewer_mac, "user_ended")
//	esp-viewer POST /esp/answer            -> MarkAnswered or MarkRejected
//
// MarkAnswered is the only CAS-style operation: a UPDATE with a
// guard (`answered_by IS NULL AND ended_at IS NULL`) returning the
// rows-affected count tells the caller whether this was the
// winning answer. Losing callers must NOT push a cancel - they
// already received one when the first answerer pushed
// answered_elsewhere.
package doorbellcalls

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Cancel reasons. Strings are written as-is into the
// doorbell_calls.cancel_reason column.
const (
	ReasonTimeout           = "timeout"
	ReasonRejected          = "rejected"
	ReasonAnsweredElsewhere = "answered_elsewhere"
	ReasonUserEnded         = "user_ended"
)

// Sentinel errors. Callers may switch on these via errors.Is.
var (
	ErrCallNotFound = errors.New("doorbellcalls: call not found")
)

// Call mirrors one row of doorbell_calls. Pointers signal NULL
// columns (every nullable column in the schema).
type Call struct {
	EventID      string
	ViewerMAC    string
	DeviceID     string
	StartedAt    time.Time
	AnsweredBy   string
	AnsweredAt   *time.Time
	EndedBy      string
	EndedAt      *time.Time
	CancelReason string
}

// Service is the typed CRUD facade.
type Service struct {
	db  *sql.DB
	now func() time.Time
}

// New constructs a Service with time.Now as the clock.
func New(db *sql.DB) *Service { return NewWithClock(db, time.Now) }

// NewWithClock injects a deterministic clock for tests.
func NewWithClock(db *sql.DB, now func() time.Time) *Service {
	return &Service{db: db, now: now}
}

// Start inserts a fresh doorbell_calls row for an incoming
// doorbell_start frame. If a row with the same event_id already
// exists (UDM re-emit, double-tap), Start is a no-op - the
// original row keeps its lifecycle and the duplicate frame is
// safely ignored.
func (s *Service) Start(ctx context.Context, eventID, viewerMAC, deviceID string) error {
	if eventID == "" {
		return errors.New("doorbellcalls: event_id required")
	}
	if viewerMAC == "" {
		return errors.New("doorbellcalls: viewer_mac required")
	}
	now := s.now().UnixMilli()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO doorbell_calls (event_id, viewer_mac, device_id, started_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(event_id) DO NOTHING`,
		eventID, viewerMAC, nullable(deviceID), now)
	if err != nil {
		return fmt.Errorf("doorbellcalls: start: %w", err)
	}
	return nil
}

// MarkAnswered is the compare-and-set winner-takes-all. Returns
// firstAnswerer=true exactly once per event_id: when the row was
// not yet answered and not yet ended. Subsequent calls (race
// losers) return firstAnswerer=false with no error.
//
// ErrCallNotFound is returned when the event_id is unknown - the
// caller can choose to treat that as "stale answer" (the row was
// already ended and the cancel pruner cleaned it up).
func (s *Service) MarkAnswered(ctx context.Context, eventID, viewerMAC string) (firstAnswerer bool, err error) {
	if eventID == "" {
		return false, errors.New("doorbellcalls: event_id required")
	}
	if viewerMAC == "" {
		return false, errors.New("doorbellcalls: viewer_mac required")
	}
	now := s.now().UnixMilli()
	res, err := s.db.ExecContext(ctx,
		`UPDATE doorbell_calls
		    SET answered_by = ?,
		        answered_at = ?
		  WHERE event_id    = ?
		    AND answered_by IS NULL
		    AND ended_at    IS NULL`,
		viewerMAC, now, eventID)
	if err != nil {
		return false, fmt.Errorf("doorbellcalls: mark answered: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 1 {
		return true, nil
	}
	// Either the row does not exist or another viewer already won.
	// Distinguish by a follow-up SELECT so callers can audit
	// "stale answer" vs "lost race".
	var found bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM doorbell_calls WHERE event_id = ?`, eventID,
	).Scan(&found); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrCallNotFound
		}
		return false, fmt.Errorf("doorbellcalls: probe: %w", err)
	}
	return false, nil
}

// MarkRejected stamps cancel_reason=rejected and ends the call.
// Idempotent: re-running on an already-ended call is a no-op.
func (s *Service) MarkRejected(ctx context.Context, eventID, viewerMAC string) error {
	return s.markEndedLocked(ctx, eventID, viewerMAC, ReasonRejected)
}

// MarkEnded stamps cancel_reason and ended_by. Idempotent on
// already-ended rows. Used by the doorbellhub for timeout-style
// cancels (viewerMAC may be empty) and by the user-ended path.
func (s *Service) MarkEnded(ctx context.Context, eventID, viewerMAC, reason string) error {
	if reason == "" {
		return errors.New("doorbellcalls: reason required")
	}
	return s.markEndedLocked(ctx, eventID, viewerMAC, reason)
}

func (s *Service) markEndedLocked(ctx context.Context, eventID, viewerMAC, reason string) error {
	if eventID == "" {
		return errors.New("doorbellcalls: event_id required")
	}
	now := s.now().UnixMilli()
	res, err := s.db.ExecContext(ctx,
		`UPDATE doorbell_calls
		    SET ended_by      = COALESCE(NULLIF(?, ''), ended_by),
		        ended_at      = COALESCE(ended_at, ?),
		        cancel_reason = COALESCE(cancel_reason, ?)
		  WHERE event_id = ?`,
		viewerMAC, now, reason, eventID)
	if err != nil {
		return fmt.Errorf("doorbellcalls: mark ended: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrCallNotFound
	}
	return nil
}

// Get returns the row for the given event_id or ErrCallNotFound.
func (s *Service) Get(ctx context.Context, eventID string) (Call, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT event_id, viewer_mac, COALESCE(device_id, ''),
		        started_at, COALESCE(answered_by, ''), answered_at,
		        COALESCE(ended_by, ''), ended_at, COALESCE(cancel_reason, '')
		   FROM doorbell_calls WHERE event_id = ?`, eventID)
	return scanCall(row)
}

// GetActive returns all calls for a viewer that are still alive
// (no ended_at). Useful for the dashboard and for the web-viewer
// "end call" button which fetches the current event_id.
func (s *Service) GetActive(ctx context.Context, viewerMAC string) ([]Call, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT event_id, viewer_mac, COALESCE(device_id, ''),
		        started_at, COALESCE(answered_by, ''), answered_at,
		        COALESCE(ended_by, ''), ended_at, COALESCE(cancel_reason, '')
		   FROM doorbell_calls
		  WHERE viewer_mac = ? AND ended_at IS NULL
		  ORDER BY started_at DESC`, viewerMAC)
	if err != nil {
		return nil, fmt.Errorf("doorbellcalls: get active: %w", err)
	}
	defer rows.Close()
	out := make([]Call, 0)
	for rows.Next() {
		c, err := scanCall(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// rowScanner is the common surface of *sql.Row and *sql.Rows
// so scanCall can be used in both Get and GetActive.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanCall(r rowScanner) (Call, error) {
	var (
		c          Call
		started    int64
		answeredAt sql.NullInt64
		endedAt    sql.NullInt64
	)
	if err := r.Scan(&c.EventID, &c.ViewerMAC, &c.DeviceID,
		&started, &c.AnsweredBy, &answeredAt,
		&c.EndedBy, &endedAt, &c.CancelReason); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Call{}, ErrCallNotFound
		}
		return Call{}, fmt.Errorf("doorbellcalls: scan: %w", err)
	}
	c.StartedAt = time.UnixMilli(started)
	if answeredAt.Valid {
		t := time.UnixMilli(answeredAt.Int64)
		c.AnsweredAt = &t
	}
	if endedAt.Valid {
		t := time.UnixMilli(endedAt.Int64)
		c.EndedAt = &t
	}
	return c, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
