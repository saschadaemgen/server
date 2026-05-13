// Package doorhistory owns the door_events table: writes when a
// doorbell event arrives, updates when a cancel matches, reads for
// the mieter and admin UIs.
//
// Saison 13-01 introduces this table (migration 005) and connects
// the doorbellhub to persist every start and cancel alongside the
// SSE fan-out. Saison 14 will add the UA-webhook receiver as a
// second writer (event_type "doorbell_unlocked", etc.); saison 16+
// will populate the prev_hash / entry_hash columns for the
// stempelkarten append-only audit chain. In saison 13 those
// columns stay NULL.
//
// The package exposes a Store interface so the doorbellhub and
// HTTP handlers can be unit-tested against a fake without spinning
// up SQLite.
package doorhistory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Event type strings written to door_events.event_type. The list
// is extended in later saisons (S13-03 adds doorbell_answered and
// doorbell_ended; S16+ adds punch_in, punch_out, visitor_enter).
const (
	TypeDoorbellStart    = "doorbell_start"
	TypeDoorbellCancel   = "doorbell_cancel"
	TypeDoorbellAnswered = "doorbell_answered"
	TypeDoorbellEnded    = "doorbell_ended"
)

// Event mirrors a row in door_events. Time pointers are nil when
// the corresponding SQL column is NULL. raw_frame is intentionally
// not exposed here; if a future consumer needs it, the Store grows
// a dedicated read method.
type Event struct {
	ID          int64
	MockMAC     string
	EventType   string
	IntercomMAC string
	OccurredAt  time.Time
	CancelledAt *time.Time
	AnsweredAt  *time.Time
	EndedAt     *time.Time
	CancelToken string
	RoomID      string
	ReadAt      *time.Time
	PrevHash    string
	EntryHash   string
}

// AdminStats aggregates door_events for the admin dashboard. The
// PerMock map is keyed by viewer_mac so callers can join against
// mock_viewers.name for display.
type AdminStats struct {
	Total24h   int
	Total7d    int
	Total30d   int
	PerMock24h map[string]int
}

// Store is the doorhistory contract. Implementations live in this
// package (sqlite) or in tests (memory fake).
type Store interface {
	Insert(ctx context.Context, ev Event, rawFrame []byte) (int64, error)
	UpdateCancel(ctx context.Context, mockMAC, cancelToken string, cancelledAt time.Time) error
	MarkRead(ctx context.Context, mockMAC string, eventIDs []int64) error
	MarkAllRead(ctx context.Context, mockMAC string, readAt time.Time) error
	ListForMock(ctx context.Context, mockMAC string, limit int) ([]Event, error)
	ListRecent(ctx context.Context, limit int) ([]Event, error)
	UnreadCount(ctx context.Context, mockMAC string) (int, error)
	CountSince(ctx context.Context, since time.Time) (int, error)
	AggregateAdmin(ctx context.Context, now time.Time) (AdminStats, error)
}

// ErrNotFound is returned by UpdateCancel when no matching open
// start row exists for (viewer_mac, cancel_token).
var ErrNotFound = errors.New("doorhistory: not found")

// SQLStore is the production Store backed by the platform sqlite
// database. Cheap to construct; safe for concurrent use because
// all access goes through database/sql.
type SQLStore struct {
	db *sql.DB
}

// NewSQLStore wires a Store against the given *sql.DB.
func NewSQLStore(db *sql.DB) *SQLStore {
	return &SQLStore{db: db}
}

// Insert persists a new door_events row and returns the generated
// id. occurred_at is taken from ev.OccurredAt (truncated to whole
// seconds). rawFrame may be empty; we write an empty BLOB rather
// than NULL so callers do not have to special-case nil.
func (s *SQLStore) Insert(ctx context.Context, ev Event, rawFrame []byte) (int64, error) {
	if ev.MockMAC == "" {
		return 0, errors.New("doorhistory: viewer_mac must not be empty")
	}
	if ev.EventType == "" {
		return 0, errors.New("doorhistory: event_type must not be empty")
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now()
	}
	if rawFrame == nil {
		rawFrame = []byte{}
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO door_events
		   (viewer_mac, event_type, intercom_mac, occurred_at,
		    cancel_token, room_id, raw_frame)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ev.MockMAC,
		ev.EventType,
		nullable(ev.IntercomMAC),
		ev.OccurredAt.Unix(),
		nullable(ev.CancelToken),
		nullable(ev.RoomID),
		rawFrame,
	)
	if err != nil {
		return 0, fmt.Errorf("doorhistory: insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("doorhistory: last insert id: %w", err)
	}
	return id, nil
}

// UpdateCancel marks the youngest open doorbell_start event for
// (viewer_mac, cancel_token) as cancelled. "Open" means
// cancelled_at IS NULL. If no row matches we return ErrNotFound;
// callers can warn-log and continue.
func (s *SQLStore) UpdateCancel(ctx context.Context, mockMAC, cancelToken string, cancelledAt time.Time) error {
	if mockMAC == "" || cancelToken == "" {
		return ErrNotFound
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE door_events
		    SET cancelled_at = ?
		  WHERE id = (
		      SELECT id FROM door_events
		       WHERE viewer_mac = ?
		         AND cancel_token = ?
		         AND cancelled_at IS NULL
		       ORDER BY occurred_at DESC
		       LIMIT 1
		  )`,
		cancelledAt.Unix(), mockMAC, cancelToken,
	)
	if err != nil {
		return fmt.Errorf("doorhistory: update cancel: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("doorhistory: rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkRead sets read_at for the listed event ids if they belong to
// mockMAC. Mock-scoping is part of the SQL so a malicious caller
// cannot mark another mock's events as read by guessing ids.
// Already-read rows are left untouched (read_at IS NULL check).
func (s *SQLStore) MarkRead(ctx context.Context, mockMAC string, eventIDs []int64) error {
	if mockMAC == "" || len(eventIDs) == 0 {
		return nil
	}
	placeholders := make([]string, len(eventIDs))
	args := make([]any, 0, len(eventIDs)+2)
	args = append(args, time.Now().Unix(), mockMAC)
	for i, id := range eventIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	q := `UPDATE door_events
	         SET read_at = ?
	       WHERE viewer_mac = ?
	         AND read_at IS NULL
	         AND id IN (` + strings.Join(placeholders, ",") + `)`
	if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("doorhistory: mark read: %w", err)
	}
	return nil
}

// MarkAllRead sets read_at on every still-unread row for mockMAC.
// Used by Variante B if the per-id approach turns out to be too
// expensive; in saison 13 the mieter handler uses MarkRead with
// the rendered list (Variante A).
func (s *SQLStore) MarkAllRead(ctx context.Context, mockMAC string, readAt time.Time) error {
	if mockMAC == "" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE door_events SET read_at = ? WHERE viewer_mac = ? AND read_at IS NULL`,
		readAt.Unix(), mockMAC,
	); err != nil {
		return fmt.Errorf("doorhistory: mark all read: %w", err)
	}
	return nil
}

// ListForMock returns up to limit most-recent events for mockMAC,
// ordered newest first. limit <= 0 falls back to 50 so callers
// cannot accidentally drag the whole table.
func (s *SQLStore) ListForMock(ctx context.Context, mockMAC string, limit int) ([]Event, error) {
	if mockMAC == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, viewer_mac, event_type, intercom_mac, occurred_at,
		        cancelled_at, answered_at, ended_at,
		        cancel_token, room_id, read_at,
		        prev_hash, entry_hash
		   FROM door_events
		  WHERE viewer_mac = ?
		  ORDER BY occurred_at DESC, id DESC
		  LIMIT ?`,
		mockMAC, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("doorhistory: query list: %w", err)
	}
	defer rows.Close()
	out := make([]Event, 0, limit)
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("doorhistory: rows: %w", err)
	}
	return out, nil
}

// UnreadCount counts door_events with read_at IS NULL for mockMAC.
// Hot path (called on every /m/ render), so the dedicated partial
// index idx_door_events_unread keeps this cheap.
func (s *SQLStore) UnreadCount(ctx context.Context, mockMAC string) (int, error) {
	if mockMAC == "" {
		return 0, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM door_events WHERE viewer_mac = ? AND read_at IS NULL`,
		mockMAC,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("doorhistory: unread count: %w", err)
	}
	return n, nil
}

// ListRecent returns up to limit most-recent doorbell_start
// events across ALL viewers, newest first. Used by the admin
// dashboard's global activity list (Saison 13-02-FIX4-a-HOTFIX3).
func (s *SQLStore) ListRecent(ctx context.Context, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, viewer_mac, event_type, intercom_mac, occurred_at,
		        cancelled_at, answered_at, ended_at,
		        cancel_token, room_id, read_at,
		        prev_hash, entry_hash
		   FROM door_events
		  WHERE event_type = ?
		  ORDER BY occurred_at DESC, id DESC
		  LIMIT ?`,
		TypeDoorbellStart, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("doorhistory: query recent: %w", err)
	}
	defer rows.Close()
	out := make([]Event, 0, limit)
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("doorhistory: rows: %w", err)
	}
	return out, nil
}

// CountSince zaehlt doorbell_start-Eintraege mit occurred_at >=
// since. Wird vom Dashboard fuer "Klingel-Events heute" und
// "Klingel-Events 7 Tage" benutzt.
func (s *SQLStore) CountSince(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM door_events
		  WHERE event_type = ? AND occurred_at >= ?`,
		TypeDoorbellStart, since.Unix(),
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("doorhistory: count since: %w", err)
	}
	return n, nil
}

// AggregateAdmin builds the dashboard payload: totals over 24h /
// 7d / 30d windows and a per-mock count for the last 24h.
func (s *SQLStore) AggregateAdmin(ctx context.Context, now time.Time) (AdminStats, error) {
	cutoff24h := now.Add(-24 * time.Hour).Unix()
	cutoff7d := now.Add(-7 * 24 * time.Hour).Unix()
	cutoff30d := now.Add(-30 * 24 * time.Hour).Unix()

	stats := AdminStats{PerMock24h: make(map[string]int)}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM door_events WHERE event_type = ? AND occurred_at >= ?`,
		TypeDoorbellStart, cutoff24h,
	).Scan(&stats.Total24h); err != nil {
		return AdminStats{}, fmt.Errorf("doorhistory: aggregate 24h: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM door_events WHERE event_type = ? AND occurred_at >= ?`,
		TypeDoorbellStart, cutoff7d,
	).Scan(&stats.Total7d); err != nil {
		return AdminStats{}, fmt.Errorf("doorhistory: aggregate 7d: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM door_events WHERE event_type = ? AND occurred_at >= ?`,
		TypeDoorbellStart, cutoff30d,
	).Scan(&stats.Total30d); err != nil {
		return AdminStats{}, fmt.Errorf("doorhistory: aggregate 30d: %w", err)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT viewer_mac, COUNT(*) AS n
		   FROM door_events
		  WHERE event_type = ?
		    AND occurred_at >= ?
		  GROUP BY viewer_mac
		  ORDER BY n DESC`,
		TypeDoorbellStart, cutoff24h,
	)
	if err != nil {
		return AdminStats{}, fmt.Errorf("doorhistory: per-mock 24h: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var mac string
		var n int
		if err := rows.Scan(&mac, &n); err != nil {
			return AdminStats{}, fmt.Errorf("doorhistory: scan per-mock: %w", err)
		}
		stats.PerMock24h[mac] = n
	}
	if err := rows.Err(); err != nil {
		return AdminStats{}, fmt.Errorf("doorhistory: per-mock rows: %w", err)
	}
	return stats, nil
}

// scanEvent maps a single row from ListForMock to an Event.
func scanEvent(rows *sql.Rows) (Event, error) {
	var (
		ev           Event
		intercomMAC  sql.NullString
		cancelledAt  sql.NullInt64
		answeredAt   sql.NullInt64
		endedAt      sql.NullInt64
		cancelToken  sql.NullString
		roomID       sql.NullString
		readAt       sql.NullInt64
		prevHash     sql.NullString
		entryHash    sql.NullString
		occurredUnix int64
	)
	if err := rows.Scan(
		&ev.ID, &ev.MockMAC, &ev.EventType, &intercomMAC, &occurredUnix,
		&cancelledAt, &answeredAt, &endedAt,
		&cancelToken, &roomID, &readAt,
		&prevHash, &entryHash,
	); err != nil {
		return Event{}, fmt.Errorf("doorhistory: scan: %w", err)
	}
	ev.OccurredAt = time.Unix(occurredUnix, 0)
	if intercomMAC.Valid {
		ev.IntercomMAC = intercomMAC.String
	}
	if cancelledAt.Valid {
		t := time.Unix(cancelledAt.Int64, 0)
		ev.CancelledAt = &t
	}
	if answeredAt.Valid {
		t := time.Unix(answeredAt.Int64, 0)
		ev.AnsweredAt = &t
	}
	if endedAt.Valid {
		t := time.Unix(endedAt.Int64, 0)
		ev.EndedAt = &t
	}
	if cancelToken.Valid {
		ev.CancelToken = cancelToken.String
	}
	if roomID.Valid {
		ev.RoomID = roomID.String
	}
	if readAt.Valid {
		t := time.Unix(readAt.Int64, 0)
		ev.ReadAt = &t
	}
	if prevHash.Valid {
		ev.PrevHash = prevHash.String
	}
	if entryHash.Valid {
		ev.EntryHash = entryHash.String
	}
	return ev, nil
}

// nullable returns a sql.NullString for the given value, mapping
// the empty string to NULL. Keeps optional columns clean instead
// of writing "".
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
