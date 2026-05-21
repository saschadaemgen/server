// Package store persists profile.Profile records in a SQLite database
// using the pure-Go modernc.org/sqlite driver (no cgo, arm64 cross-
// compile unchanged).
//
// The store is intentionally narrow: profile CRUD only. Cameras come
// from the Protect API at request time (no caching here today); session
// / consumer state lives in memory in the hub layer.
//
// Seed semantics (S5-01):
//
//   - On the first start the DB is empty. If the caller passes a seed
//     list (typically from CARVILON_PROFILES_JSON), [Store.SeedIfEmpty]
//     copies it into the DB once. Subsequent starts find the DB
//     non-empty and ignore the seed list — the DB becomes the single
//     source of truth so a later "I changed the JSON but it still loads
//     the old values" mystery cannot happen.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"

	"carvilon.local/stream/internal/profile"
)

// ErrNotFound is returned by [Store.Get] and [Store.Delete] when no
// row matches the given name.
var ErrNotFound = errors.New("store: profile not found")

// Store wraps a SQLite handle holding the streaming-server profile
// table. Safe for concurrent use; the underlying *sql.DB is the
// synchronisation point.
type Store struct {
	db   *sql.DB
	path string
}

// Open creates or opens the SQLite file at path and ensures the
// schema exists. The parent directory is created if needed.
//
// path = ":memory:" gives an in-memory DB (used by tests).
func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("store: create dir %q: %w", dir, err)
			}
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open %q: %w", path, err)
	}
	// modernc.org/sqlite uses one connection per call; cap it for safety.
	db.SetMaxOpenConns(4)

	// Pragmas. WAL + foreign-keys are standard hardenings; we have no
	// FKs today but WAL helps for read concurrency with periodic writes
	// from the admin UI.
	for _, stmt := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=NORMAL`,
		`PRAGMA foreign_keys=ON`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("store: pragma %q: %w", stmt, err)
		}
	}

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: schema: %w", err)
	}

	return &Store{db: db, path: path}, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS profiles (
	name        TEXT PRIMARY KEY,
	camera_id   TEXT NOT NULL,
	quality     TEXT NOT NULL,
	usage       TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT ''
);
`

// Path returns the configured filesystem path. ":memory:" for tests.
func (s *Store) Path() string { return s.path }

// Close shuts the underlying connection. Idempotent — wrap *Store's
// lifetime, e.g. via defer, but a double-Close is safe-ish (sql.DB
// returns nil on second Close).
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Put creates or updates one profile. Validates via profile.Validate
// before touching the DB.
func (s *Store) Put(ctx context.Context, p profile.Profile) error {
	if err := p.Validate(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO profiles (name, camera_id, quality, usage, description)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			camera_id   = excluded.camera_id,
			quality     = excluded.quality,
			usage       = excluded.usage,
			description = excluded.description
	`, p.Name, p.CameraID, string(p.Quality), string(p.Usage), p.Description)
	if err != nil {
		return fmt.Errorf("store: put %q: %w", p.Name, err)
	}
	return nil
}

// Get returns the profile with the given name, or [ErrNotFound].
func (s *Store) Get(ctx context.Context, name string) (profile.Profile, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT name, camera_id, quality, usage, description
		FROM profiles WHERE name = ?
	`, name)
	var p profile.Profile
	var q, u string
	if err := row.Scan(&p.Name, &p.CameraID, &q, &u, &p.Description); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return profile.Profile{}, ErrNotFound
		}
		return profile.Profile{}, fmt.Errorf("store: get %q: %w", name, err)
	}
	p.Quality = profile.Quality(q)
	p.Usage = profile.Usage(u)
	return p, nil
}

// Delete removes the profile with the given name. Returns
// [ErrNotFound] if no row was deleted (so callers can map to 404).
func (s *Store) Delete(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM profiles WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("store: delete %q: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete %q: rows affected: %w", name, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// List returns all profiles sorted by name.
func (s *Store) List(ctx context.Context) ([]profile.Profile, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, camera_id, quality, usage, description
		FROM profiles ORDER BY name
	`)
	if err != nil {
		return nil, fmt.Errorf("store: list: %w", err)
	}
	defer rows.Close()
	var out []profile.Profile
	for rows.Next() {
		var p profile.Profile
		var q, u string
		if err := rows.Scan(&p.Name, &p.CameraID, &q, &u, &p.Description); err != nil {
			return nil, fmt.Errorf("store: list scan: %w", err)
		}
		p.Quality = profile.Quality(q)
		p.Usage = profile.Usage(u)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list rows: %w", err)
	}
	return out, nil
}

// Count returns the number of profiles in the DB.
func (s *Store) Count(ctx context.Context) (int, error) {
	var n int
	row := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM profiles`)
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count: %w", err)
	}
	return n, nil
}

// SeedIfEmpty inserts the given profiles into the DB iff the DB is
// currently empty. Returns the number of rows inserted (0 if the DB
// was already populated). Each profile is Validate'd before insertion.
//
// This is the S5-01 "JSON only seeds an empty DB; the DB is the
// truth thereafter" rule, made explicit and testable.
func (s *Store) SeedIfEmpty(ctx context.Context, ps []profile.Profile) (int, error) {
	n, err := s.Count(ctx)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		return 0, nil
	}
	if len(ps) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: seed begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO profiles (name, camera_id, quality, usage, description)
		VALUES (?, ?, ?, ?, ?)
	`)
	if err != nil {
		return 0, fmt.Errorf("store: seed prepare: %w", err)
	}
	defer stmt.Close()

	inserted := 0
	for _, p := range ps {
		if err := p.Validate(); err != nil {
			return 0, fmt.Errorf("store: seed validate %q: %w", p.Name, err)
		}
		if _, err := stmt.ExecContext(ctx, p.Name, p.CameraID, string(p.Quality), string(p.Usage), p.Description); err != nil {
			return 0, fmt.Errorf("store: seed insert %q: %w", p.Name, err)
		}
		inserted++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: seed commit: %w", err)
	}
	return inserted, nil
}
