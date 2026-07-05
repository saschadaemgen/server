// Package readerstore is the persistence layer for the reader
// registry (migration 036): the device-level record of every tag
// reader the host has detected. It is the single SQL writer for the
// readers table and the source of truth behind the NFC menu page.
//
// A reader is registered automatically at startup from the driver's
// runtime detection (one source, two views: this registry is the
// component view, the editor palette is the logic-block view - see the
// nfc driver). Registration is reconciling: a detected reader is
// upserted online, a previously known reader that is no longer detected
// stays as an offline row (never deleted - a missing device must be
// visible, not silently gone), and it flips back to online when it
// reappears. Identity is stable across restarts so a second start of
// the same hardware updates the same row instead of creating a
// duplicate.
//
// The registry is deliberately reader-type agnostic (the `kind`
// column): the PN532/NFC reader is the first and only type today, but
// the Sync/List seam takes a neutral Detected value so a second reader
// type (a UA reader, its own ticket) is a pure addition, not a rewrite.
package readerstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound is returned when a reader id has no row.
var ErrNotFound = errors.New("readerstore: reader not found")

// Store is the SQL gateway for the reader registry.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Option mutates a Store during construction.
type Option func(*Store)

// WithClock injects a test clock.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// New constructs a Store.
func New(db *sql.DB, opts ...Option) *Store {
	s := &Store{db: db, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Detected is one reader a driver reports from its startup detection,
// in a neutral shape the registry understands without knowing about any
// concrete reader model. ID is the stable identity (e.g. "nfc:i2c-1");
// Name is the human label that also names the reader's System/Reader
// graph.
type Detected struct {
	ID       string
	Kind     string // reader modality, e.g. "nfc"
	Model    string // e.g. "PN532"
	Firmware string
	Bus      string // e.g. "i2c-1"
	Name     string // display + graph name
}

// Reader is a read view of one registry row.
type Reader struct {
	ID          string
	Kind        string
	Model       string
	Firmware    string
	Bus         string
	Name        string
	Online      bool
	GraphID     int64  // 0 when no System/Reader graph is linked
	LastUID     string // last read tag, canonical form; "" if never
	LastSeenAt  int64  // ms epoch of the last tag read; 0 if never
	FirstSeenAt int64  // ms epoch of first registration
	UpdatedAt   int64
}

// Sync reconciles the registry against the readers a driver detected at
// startup. For each detected reader it upserts an online row (creating
// the row and, on first sight, a System/Reader graph via ensureGraph so
// the NFC page's editor jump has a target); every previously online
// reader that is NOT in the detected set is flipped offline but kept.
// last_uid / last_seen_at / first_seen_at are preserved across syncs so
// a reader's history survives a restart.
//
// ensureGraph is injected (rather than depending on designerstore
// directly) so the store stays a pure SQL gateway and unit-testable in
// isolation; it must be idempotent (return the same graph id for the
// same name) and may return 0 to link no graph. It is only called for a
// reader that has no linked graph yet, so a normal restart makes no new
// graphs.
//
// Not wrapped in a single transaction on purpose: ensureGraph runs its
// own transaction on the same connection, and nesting a writer around
// it would self-deadlock SQLite. Sync runs once at startup,
// single-threaded, so per-statement atomicity is sufficient.
func (s *Store) Sync(ctx context.Context, detected []Detected, ensureGraph func(ctx context.Context, name string) (int64, error)) error {
	now := s.now().UnixMilli()
	seen := make([]string, 0, len(detected))
	for _, d := range detected {
		if d.ID == "" {
			continue
		}
		seen = append(seen, d.ID)

		var graphID sql.NullInt64
		exists := true
		err := s.db.QueryRowContext(ctx, `SELECT graph_id FROM readers WHERE id = ?`, d.ID).Scan(&graphID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			exists = false
		case err != nil:
			return fmt.Errorf("readerstore: lookup %q: %w", d.ID, err)
		}
		// Ensure a linked graph exactly once (first registration, or a
		// row whose link was cleared): a normal restart reuses the id.
		if !graphID.Valid || graphID.Int64 == 0 {
			gid, err := ensureGraph(ctx, d.Name)
			if err != nil {
				return fmt.Errorf("readerstore: ensure graph for %q: %w", d.ID, err)
			}
			graphID = sql.NullInt64{Int64: gid, Valid: gid != 0}
		}

		if exists {
			_, err = s.db.ExecContext(ctx,
				`UPDATE readers SET kind = ?, model = ?, firmware = ?, bus = ?, name = ?,
				        online = 1, graph_id = ?, updated_at = ? WHERE id = ?`,
				d.Kind, d.Model, d.Firmware, d.Bus, d.Name, graphID, now, d.ID)
		} else {
			_, err = s.db.ExecContext(ctx,
				`INSERT INTO readers (id, kind, model, firmware, bus, name, online, graph_id,
				        last_uid, last_seen_at, first_seen_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, 1, ?, '', NULL, ?, ?)`,
				d.ID, d.Kind, d.Model, d.Firmware, d.Bus, d.Name, graphID, now, now)
		}
		if err != nil {
			return fmt.Errorf("readerstore: upsert %q: %w", d.ID, err)
		}
	}

	// Everything that was online but is no longer detected goes offline,
	// kept in place. Empty detected set => all online rows go offline.
	if len(seen) == 0 {
		_, err := s.db.ExecContext(ctx,
			`UPDATE readers SET online = 0, updated_at = ? WHERE online = 1`, now)
		if err != nil {
			return fmt.Errorf("readerstore: mark all offline: %w", err)
		}
		return nil
	}
	args := make([]any, 0, len(seen)+1)
	args = append(args, now)
	for _, id := range seen {
		args = append(args, id)
	}
	q := `UPDATE readers SET online = 0, updated_at = ? WHERE online = 1 AND id NOT IN (` +
		placeholders(len(seen)) + `)`
	if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("readerstore: mark offline: %w", err)
	}
	return nil
}

// NoteTag records the last tag a reader saw (called from the driver's
// tag observer during a run). A tag for an unregistered reader is a
// no-op rather than an error - the registry only tracks known readers.
func (s *Store) NoteTag(ctx context.Context, id, uid string) error {
	now := s.now().UnixMilli()
	_, err := s.db.ExecContext(ctx,
		`UPDATE readers SET last_uid = ?, last_seen_at = ?, updated_at = ? WHERE id = ?`,
		uid, now, now, id)
	if err != nil {
		return fmt.Errorf("readerstore: note tag for %q: %w", id, err)
	}
	return nil
}

// List returns all registered readers, offline ones included, ordered
// for stable display (name, then id).
func (s *Store) List(ctx context.Context) ([]Reader, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, model, firmware, bus, name, online, COALESCE(graph_id, 0),
		        last_uid, COALESCE(last_seen_at, 0), first_seen_at, updated_at
		   FROM readers ORDER BY name, id`)
	if err != nil {
		return nil, fmt.Errorf("readerstore: list: %w", err)
	}
	defer rows.Close()
	var out []Reader
	for rows.Next() {
		var r Reader
		if err := rows.Scan(&r.ID, &r.Kind, &r.Model, &r.Firmware, &r.Bus, &r.Name,
			&r.Online, &r.GraphID, &r.LastUID, &r.LastSeenAt, &r.FirstSeenAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("readerstore: scan reader: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("readerstore: list rows: %w", err)
	}
	return out, nil
}

// Get returns one reader by id.
func (s *Store) Get(ctx context.Context, id string) (Reader, error) {
	var r Reader
	err := s.db.QueryRowContext(ctx,
		`SELECT id, kind, model, firmware, bus, name, online, COALESCE(graph_id, 0),
		        last_uid, COALESCE(last_seen_at, 0), first_seen_at, updated_at
		   FROM readers WHERE id = ?`, id).
		Scan(&r.ID, &r.Kind, &r.Model, &r.Firmware, &r.Bus, &r.Name,
			&r.Online, &r.GraphID, &r.LastUID, &r.LastSeenAt, &r.FirstSeenAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Reader{}, ErrNotFound
	}
	if err != nil {
		return Reader{}, fmt.Errorf("readerstore: get %q: %w", id, err)
	}
	return r, nil
}

// placeholders returns "?, ?, ?" for n>0.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}
