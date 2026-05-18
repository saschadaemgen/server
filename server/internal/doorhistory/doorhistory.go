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
//
// Saison 14-04-Phase2: HiddenByViewer is set when a row is joined
// against viewer_hidden_events for the calling mac (admin-side
// read). HiddenAt is the corresponding hidden_at timestamp.
// Mieter-side reads filter the row out entirely; only AdminListAll
// surfaces the flag.
type Event struct {
	ID             int64
	MockMAC        string
	EventType      string
	IntercomMAC    string
	OccurredAt     time.Time
	CancelledAt    *time.Time
	AnsweredAt     *time.Time
	EndedAt        *time.Time
	CancelToken    string
	RoomID         string
	ReadAt         *time.Time
	PrevHash       string
	EntryHash      string
	HiddenByViewer bool
	HiddenAt       *time.Time
}

// ListOpts bundles the pagination + date-range filter for the
// saison-14-04-phase2 listing endpoints. Zero values default to
// "no bound": Limit 0 falls back to 20, Offset 0 starts at the
// newest row, From/To zero-time means no lower/upper cutoff.
// Limit is clamped to ListOptsMaxLimit so a stray client cannot
// pull the whole table in one request.
type ListOpts struct {
	Limit  int
	Offset int
	From   time.Time
	To     time.Time
}

// ListOptsMaxLimit caps how many rows a single ListVisible /
// AdminListAll call returns. Handlers can pre-clamp the query
// parameter; the store re-clamps defensively.
const ListOptsMaxLimit = 50

// AdminListResult is the envelope AdminListAll returns. Events
// is the requested page (admin sees hidden ones too, flagged),
// TotalCount is the count of ALL door_events rows for the viewer
// regardless of hidden state, HiddenCount is the subset that the
// mieter has soft-deleted, HasMore signals whether another page
// is available beyond this offset+limit.
type AdminListResult struct {
	Events      []Event
	TotalCount  int
	HiddenCount int
	HasMore     bool
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
//
// Saison 14-04-Phase2 adds the soft-delete + admin-hard-delete +
// pagination contract. ListVisible is the mieter-side equivalent
// of ListForMock that respects viewer_hidden_events; ListForMock
// stays as the unfiltered legacy reader (used by handler_home.go
// at server-render time, where the existing Variante-A mark-read
// flow expects the full list).
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
	// Saison 14-04-Phase2.
	HideEvent(ctx context.Context, mockMAC string, eventID int64) error
	HideAllEvents(ctx context.Context, mockMAC string) (int, error)
	ListVisible(ctx context.Context, mockMAC string, opts ListOpts) ([]Event, error)
	CountVisible(ctx context.Context, mockMAC string, opts ListOpts) (int, error)
	AdminListAll(ctx context.Context, mockMAC string, opts ListOpts) (AdminListResult, error)
	AdminDeleteEvent(ctx context.Context, mockMAC string, eventID int64) error
	AdminDeleteAllForViewer(ctx context.Context, mockMAC string) (int, error)
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

// ---------- Saison 14-04-Phase2: soft-delete + pagination ----------

// HideEvent records a mieter-soft-delete on a single door_events
// row. Idempotent via ON CONFLICT - calling it twice for the same
// (viewer_mac, event_id) is a no-op. Mock-scoping is enforced via
// a sub-select against door_events so a malicious caller cannot
// hide another mock's events by guessing ids.
//
// Returns ErrNotFound when the event_id does not belong to
// mockMAC (or does not exist at all). Tests rely on this so a
// missing-id attempt does not silently succeed.
func (s *SQLStore) HideEvent(ctx context.Context, mockMAC string, eventID int64) error {
	if mockMAC == "" || eventID == 0 {
		return ErrNotFound
	}
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO viewer_hidden_events (viewer_mac, event_id, hidden_at)
		 SELECT ?, id, ? FROM door_events
		  WHERE id = ? AND viewer_mac = ?
		 ON CONFLICT (viewer_mac, event_id) DO NOTHING`,
		mockMAC, now, eventID, mockMAC,
	)
	if err != nil {
		return fmt.Errorf("doorhistory: hide event: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("doorhistory: hide event rows: %w", err)
	}
	if rows == 0 {
		// Could be either "event_id does not belong to mockMAC" or
		// "already hidden". Distinguish via an existence check to
		// keep idempotent calls quiet.
		var exists int
		_ = s.db.QueryRowContext(ctx,
			`SELECT 1 FROM viewer_hidden_events
			  WHERE viewer_mac = ? AND event_id = ?`,
			mockMAC, eventID,
		).Scan(&exists)
		if exists == 1 {
			return nil
		}
		return ErrNotFound
	}
	return nil
}

// HideAllEvents hides every door_events row that is currently
// visible to mockMAC. Returns the count of newly-hidden rows
// (already-hidden rows are skipped via the LEFT JOIN guard).
func (s *SQLStore) HideAllEvents(ctx context.Context, mockMAC string) (int, error) {
	if mockMAC == "" {
		return 0, nil
	}
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO viewer_hidden_events (viewer_mac, event_id, hidden_at)
		 SELECT de.viewer_mac, de.id, ?
		   FROM door_events de
		   LEFT JOIN viewer_hidden_events vhe
		          ON vhe.viewer_mac = de.viewer_mac AND vhe.event_id = de.id
		  WHERE de.viewer_mac = ? AND vhe.event_id IS NULL`,
		now, mockMAC,
	)
	if err != nil {
		return 0, fmt.Errorf("doorhistory: hide all: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("doorhistory: hide all rows: %w", err)
	}
	return int(n), nil
}

// ListVisible returns the paginated set of door_events that the
// mieter on mockMAC has NOT soft-hidden. Filter semantics:
//
//   - opts.Limit  clamped to [1, ListOptsMaxLimit]; 0 -> 20
//   - opts.Offset clamped to [0, +inf); 0 starts at newest
//   - opts.From   zero-time -> no lower cutoff (occurred_at >= from)
//   - opts.To     zero-time -> no upper cutoff (occurred_at <= to+1d)
//
// Newest-first ordering. To-bound is inclusive over the WHOLE day
// (we add 24h so a "to=2026-05-17" filter catches every event of
// that day regardless of the actual seconds).
func (s *SQLStore) ListVisible(ctx context.Context, mockMAC string, opts ListOpts) ([]Event, error) {
	if mockMAC == "" {
		return nil, nil
	}
	limit, offset := normalizeListOpts(opts)
	q, args := buildVisibleQuery(mockMAC, opts, limit, offset)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("doorhistory: list visible: %w", err)
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
		return nil, fmt.Errorf("doorhistory: list visible rows: %w", err)
	}
	return out, nil
}

// CountVisible returns the total number of door_events that
// ListVisible would surface for the given opts (ignoring
// Limit/Offset). Used by the mieter pagination logic to decide
// whether to render the "Mehr laden"-button.
func (s *SQLStore) CountVisible(ctx context.Context, mockMAC string, opts ListOpts) (int, error) {
	if mockMAC == "" {
		return 0, nil
	}
	q := `SELECT COUNT(*)
	        FROM door_events de
	        LEFT JOIN viewer_hidden_events vhe
	               ON vhe.viewer_mac = de.viewer_mac AND vhe.event_id = de.id
	       WHERE de.viewer_mac = ?
	         AND vhe.event_id IS NULL`
	args := []any{mockMAC}
	if !opts.From.IsZero() {
		q += " AND de.occurred_at >= ?"
		args = append(args, opts.From.Unix())
	}
	if !opts.To.IsZero() {
		q += " AND de.occurred_at <= ?"
		args = append(args, endOfDayUnix(opts.To))
	}
	var n int
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("doorhistory: count visible: %w", err)
	}
	return n, nil
}

func normalizeListOpts(opts ListOpts) (limit, offset int) {
	limit = opts.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > ListOptsMaxLimit {
		limit = ListOptsMaxLimit
	}
	offset = opts.Offset
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// endOfDayUnix returns the Unix timestamp of the end-of-day
// (23:59:59) for the given date. Used to make the To-bound
// inclusive over the whole day; the user's "bis 17.05." filter
// should include every event of 17 May regardless of the time
// portion the date-picker shipped.
func endOfDayUnix(t time.Time) int64 {
	end := time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, t.Location())
	return end.Unix()
}

// buildVisibleQuery composes the mieter ListVisible SQL with
// optional date-range predicates. The query shape stays parameter-
// safe; no string concatenation of user input.
func buildVisibleQuery(mockMAC string, opts ListOpts, limit, offset int) (string, []any) {
	q := `SELECT de.id, de.viewer_mac, de.event_type, de.intercom_mac,
	             de.occurred_at, de.cancelled_at, de.answered_at,
	             de.ended_at, de.cancel_token, de.room_id, de.read_at,
	             de.prev_hash, de.entry_hash
	        FROM door_events de
	        LEFT JOIN viewer_hidden_events vhe
	               ON vhe.viewer_mac = de.viewer_mac AND vhe.event_id = de.id
	       WHERE de.viewer_mac = ?
	         AND vhe.event_id IS NULL`
	args := []any{mockMAC}
	if !opts.From.IsZero() {
		q += " AND de.occurred_at >= ?"
		args = append(args, opts.From.Unix())
	}
	if !opts.To.IsZero() {
		q += " AND de.occurred_at <= ?"
		args = append(args, endOfDayUnix(opts.To))
	}
	q += " ORDER BY de.occurred_at DESC, de.id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	return q, args
}

// ---------- Admin reads + hard-delete (Saison 14-04-Phase2) ----------

// AdminListAll returns the page-of-events for mockMAC as the
// admin sees it: hidden rows are INCLUDED but flagged via
// HiddenByViewer + HiddenAt so the template can render the
// eye-off-icon + tooltip. TotalCount and HiddenCount in the
// envelope let the per-viewer admin page show the
// "84 Eintraege - 12 vom Mieter ausgeblendet"-line. HasMore is
// (offset+len(events)) < TotalCount.
//
// Admins always see the full audit trail; the only thing
// HiddenByViewer changes is rendering.
func (s *SQLStore) AdminListAll(ctx context.Context, mockMAC string, opts ListOpts) (AdminListResult, error) {
	if mockMAC == "" {
		return AdminListResult{Events: []Event{}}, nil
	}
	limit, offset := normalizeListOpts(opts)
	q, args := buildAdminQuery(mockMAC, opts, limit, offset)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return AdminListResult{}, fmt.Errorf("doorhistory: admin list: %w", err)
	}
	defer rows.Close()
	events := make([]Event, 0, limit)
	for rows.Next() {
		ev, hiddenAt, err := scanAdminEvent(rows)
		if err != nil {
			return AdminListResult{}, err
		}
		if hiddenAt.Valid {
			t := time.Unix(hiddenAt.Int64, 0)
			ev.HiddenByViewer = true
			ev.HiddenAt = &t
		}
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return AdminListResult{}, fmt.Errorf("doorhistory: admin list rows: %w", err)
	}
	// Total + Hidden in einer einzigen Aggregat-Query. Filter
	// (Date-Range) wird beruecksichtigt damit "84 Eintraege" sich
	// auf das aktuell geladene Filter-Fenster bezieht.
	total, hidden, err := s.adminCounts(ctx, mockMAC, opts)
	if err != nil {
		return AdminListResult{}, err
	}
	return AdminListResult{
		Events:      events,
		TotalCount:  total,
		HiddenCount: hidden,
		HasMore:     offset+len(events) < total,
	}, nil
}

func (s *SQLStore) adminCounts(ctx context.Context, mockMAC string, opts ListOpts) (total, hidden int, err error) {
	q := `SELECT
	          COUNT(*),
	          SUM(CASE WHEN vhe.event_id IS NOT NULL THEN 1 ELSE 0 END)
	      FROM door_events de
	      LEFT JOIN viewer_hidden_events vhe
	             ON vhe.viewer_mac = de.viewer_mac AND vhe.event_id = de.id
	      WHERE de.viewer_mac = ?`
	args := []any{mockMAC}
	if !opts.From.IsZero() {
		q += " AND de.occurred_at >= ?"
		args = append(args, opts.From.Unix())
	}
	if !opts.To.IsZero() {
		q += " AND de.occurred_at <= ?"
		args = append(args, endOfDayUnix(opts.To))
	}
	var totalRaw sql.NullInt64
	var hiddenRaw sql.NullInt64
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&totalRaw, &hiddenRaw); err != nil {
		return 0, 0, fmt.Errorf("doorhistory: admin counts: %w", err)
	}
	return int(totalRaw.Int64), int(hiddenRaw.Int64), nil
}

// AdminDeleteEvent hard-deletes one door_events row scoped by
// mockMAC so a stray ID guess cannot wipe another viewer's audit
// trail. FK CASCADE on viewer_hidden_events purges the matching
// hidden-marker automatically.
//
// Returns ErrNotFound when the row does not exist or does not
// belong to mockMAC.
func (s *SQLStore) AdminDeleteEvent(ctx context.Context, mockMAC string, eventID int64) error {
	if mockMAC == "" || eventID == 0 {
		return ErrNotFound
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM door_events WHERE id = ? AND viewer_mac = ?`,
		eventID, mockMAC,
	)
	if err != nil {
		return fmt.Errorf("doorhistory: admin delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("doorhistory: admin delete rows: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// AdminDeleteAllForViewer hard-deletes every door_events row for
// mockMAC. Returns the count of deleted rows so the admin UI can
// surface "84 Eintraege geloescht". FK CASCADE removes the
// hidden-markers in the same DELETE.
func (s *SQLStore) AdminDeleteAllForViewer(ctx context.Context, mockMAC string) (int, error) {
	if mockMAC == "" {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM door_events WHERE viewer_mac = ?`, mockMAC)
	if err != nil {
		return 0, fmt.Errorf("doorhistory: admin delete all: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("doorhistory: admin delete all rows: %w", err)
	}
	return int(n), nil
}

func buildAdminQuery(mockMAC string, opts ListOpts, limit, offset int) (string, []any) {
	q := `SELECT de.id, de.viewer_mac, de.event_type, de.intercom_mac,
	             de.occurred_at, de.cancelled_at, de.answered_at,
	             de.ended_at, de.cancel_token, de.room_id, de.read_at,
	             de.prev_hash, de.entry_hash,
	             vhe.hidden_at
	        FROM door_events de
	        LEFT JOIN viewer_hidden_events vhe
	               ON vhe.viewer_mac = de.viewer_mac AND vhe.event_id = de.id
	       WHERE de.viewer_mac = ?`
	args := []any{mockMAC}
	if !opts.From.IsZero() {
		q += " AND de.occurred_at >= ?"
		args = append(args, opts.From.Unix())
	}
	if !opts.To.IsZero() {
		q += " AND de.occurred_at <= ?"
		args = append(args, endOfDayUnix(opts.To))
	}
	q += " ORDER BY de.occurred_at DESC, de.id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	return q, args
}

// scanAdminEvent is the AdminListAll-specific row scan: identical
// to scanEvent but pulls the joined viewer_hidden_events.hidden_at
// into a NullInt64 the caller hydrates into HiddenByViewer + HiddenAt.
func scanAdminEvent(rows *sql.Rows) (Event, sql.NullInt64, error) {
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
		hiddenAt     sql.NullInt64
	)
	if err := rows.Scan(
		&ev.ID, &ev.MockMAC, &ev.EventType, &intercomMAC, &occurredUnix,
		&cancelledAt, &answeredAt, &endedAt,
		&cancelToken, &roomID, &readAt,
		&prevHash, &entryHash,
		&hiddenAt,
	); err != nil {
		return Event{}, sql.NullInt64{}, fmt.Errorf("doorhistory: admin scan: %w", err)
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
	return ev, hiddenAt, nil
}
