// Package db opens the carvilon-server sqlite database and applies
// pending schema migrations. The database stores platform data:
// admin users, mock viewers, magic-link tokens, and the two session
// tables (mieter_sessions, admin_sessions). Since Saison 12-06 the
// tenant routing key is the mock viewer's MAC; tenant identity in
// the UniFi Access Developer API is administered separately.
package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// DB wraps sql.DB plus the on-disk path for diagnostics and tests.
type DB struct {
	*sql.DB
	path string
}

// Open opens the sqlite database at path, enables WAL journaling
// and foreign-key enforcement, then applies pending migrations.
// The file is created if it does not exist. Caller must Close.
func Open(path string) (*DB, error) {
	if path == "" {
		return nil, fmt.Errorf("db: path must not be empty")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("db: mkdir parent %s: %w", dir, err)
		}
	}
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("db: open %s: %w", path, err)
	}
	// SQLite is a file-backed engine. Holding a single connection
	// keeps per-connection pragmas (foreign_keys) effective and
	// avoids "database is locked" surprises under contention.
	// Platform write volume is low enough that this is not a
	// performance concern in saison 12.
	sqlDB.SetMaxOpenConns(1)

	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("db: ping %s: %w", path, err)
	}
	if _, err := sqlDB.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("db: set journal_mode=WAL: %w", err)
	}
	if _, err := sqlDB.Exec("PRAGMA foreign_keys=ON"); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("db: set foreign_keys=ON: %w", err)
	}
	if err := applyMigrations(sqlDB); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("db: apply migrations: %w", err)
	}
	return &DB{DB: sqlDB, path: path}, nil
}

// Path returns the on-disk path passed to Open.
func (d *DB) Path() string { return d.path }
