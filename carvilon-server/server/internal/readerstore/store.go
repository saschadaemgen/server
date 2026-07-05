// Package readerstore is the persistence layer for the reader
// registry (migrations 036/037): the device-level record of every tag
// reader the host has detected. It is the single SQL writer for the
// readers table and the source of truth behind the NFC menu page and
// the palette blocks' display names.
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
// duplicate. A reader is NOT a graph in the folder tree - it is a
// palette block plus a row here.
//
// name holds the speaking auto-name (Platform-Function-Model (Bus));
// custom_name is the operator's optional override from the NFC menu.
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
// Name is the speaking auto-name.
type Detected struct {
	ID       string
	Kind     string // reader modality, e.g. "nfc"
	Model    string // e.g. "PN532"
	Firmware string
	Bus      string // e.g. "i2c-1"
	Name     string // speaking auto-name, e.g. "RPi-NFC-PN532 (I2C-1)"
}

// Reader is a read view of one registry row.
type Reader struct {
	ID          string
	Kind        string
	Model       string
	Firmware    string
	Bus         string
	Name        string // speaking auto-name
	CustomName  string // operator override; "" when unset
	Online      bool
	LastUID     string // last read tag, canonical form; "" if never
	LastSeenAt  int64  // ms epoch of the last tag read; 0 if never
	FirstSeenAt int64  // ms epoch of first registration
	UpdatedAt   int64
}

// DisplayName is the operator's custom name if set, else the auto-name.
func (r Reader) DisplayName() string {
	if r.CustomName != "" {
		return r.CustomName
	}
	return r.Name
}

// Sync reconciles the registry against the readers a driver detected at
// startup. For each detected reader it upserts an online row (preserving
// the operator's custom name, last tag and first-seen time); every
// previously online reader that is NOT in the detected set is flipped
// offline but kept. Single-threaded at startup, per-statement atomicity
// is sufficient.
func (s *Store) Sync(ctx context.Context, detected []Detected) error {
	now := s.now().UnixMilli()
	seen := make([]string, 0, len(detected))
	for _, d := range detected {
		if d.ID == "" {
			continue
		}
		seen = append(seen, d.ID)

		exists := true
		err := s.db.QueryRowContext(ctx, `SELECT 1 FROM readers WHERE id = ?`, d.ID).Scan(new(int))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			exists = false
		case err != nil:
			return fmt.Errorf("readerstore: lookup %q: %w", d.ID, err)
		}

		if exists {
			// custom_name / last_uid / last_seen_at / first_seen_at are
			// deliberately left untouched so they survive the restart.
			_, err = s.db.ExecContext(ctx,
				`UPDATE readers SET kind = ?, model = ?, firmware = ?, bus = ?, name = ?,
				        online = 1, updated_at = ? WHERE id = ?`,
				d.Kind, d.Model, d.Firmware, d.Bus, d.Name, now, d.ID)
		} else {
			_, err = s.db.ExecContext(ctx,
				`INSERT INTO readers (id, kind, model, firmware, bus, name, custom_name, online,
				        last_uid, last_seen_at, first_seen_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, '', 1, '', NULL, ?, ?)`,
				d.ID, d.Kind, d.Model, d.Firmware, d.Bus, d.Name, now, now)
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
// tag observer, continuously - a run is not required). A tag for an
// unregistered reader is a no-op rather than an error.
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

// SetCustomName sets (or clears, with "") the operator-chosen name for a
// reader; when set it overrides the auto-name in the palette and on the
// NFC page. Returns ErrNotFound for an unknown reader.
func (s *Store) SetCustomName(ctx context.Context, id, name string) error {
	name = strings.TrimSpace(name)
	res, err := s.db.ExecContext(ctx,
		`UPDATE readers SET custom_name = ?, updated_at = ? WHERE id = ?`,
		name, s.now().UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("readerstore: set custom name %q: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// List returns all registered readers, offline ones included, ordered
// for stable display (name, then id).
func (s *Store) List(ctx context.Context) ([]Reader, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, model, firmware, bus, name, custom_name, online,
		        last_uid, COALESCE(last_seen_at, 0), first_seen_at, updated_at
		   FROM readers ORDER BY name, id`)
	if err != nil {
		return nil, fmt.Errorf("readerstore: list: %w", err)
	}
	defer rows.Close()
	var out []Reader
	for rows.Next() {
		r, err := scanReader(rows)
		if err != nil {
			return nil, err
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
	row := s.db.QueryRowContext(ctx,
		`SELECT id, kind, model, firmware, bus, name, custom_name, online,
		        last_uid, COALESCE(last_seen_at, 0), first_seen_at, updated_at
		   FROM readers WHERE id = ?`, id)
	r, err := scanReader(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Reader{}, ErrNotFound
	}
	if err != nil {
		return Reader{}, fmt.Errorf("readerstore: get %q: %w", id, err)
	}
	return r, nil
}

// scanRow is the row shape shared by QueryRow and Query rows.
type scanRow interface {
	Scan(dest ...any) error
}

func scanReader(row scanRow) (Reader, error) {
	var r Reader
	err := row.Scan(&r.ID, &r.Kind, &r.Model, &r.Firmware, &r.Bus, &r.Name, &r.CustomName,
		&r.Online, &r.LastUID, &r.LastSeenAt, &r.FirstSeenAt, &r.UpdatedAt)
	return r, err
}

// placeholders returns "?, ?, ?" for n>0.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}
