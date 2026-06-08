package db

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

var migrationFilenameRE = regexp.MustCompile(`^(\d{3})_.+\.sql$`)

// applyMigrations runs every migration file under migrations/ whose
// three-digit prefix is greater than the current MAX(version) in
// schema_version. Each file is executed in a single transaction.
// Migration files own their own INSERT INTO schema_version, so the
// runner only checks applicability and never inserts on its own.
//
// On first run schema_version does not exist yet, which is treated
// as version 0.
//
// Foreign-key handling: Migrations 002 and 004 do table-rebuilds
// (CREATE new / INSERT SELECT / DROP / RENAME) on tables that
// other tables reference via FK. With foreign_keys=ON those DROPs
// would trigger ON DELETE CASCADE on the child rows and corrupt
// the data set. The published SQLite pattern is to turn the PRAGMA
// off outside the transaction, run the migration, run
// foreign_key_check after to verify the schema is still
// consistent, and turn the PRAGMA back on. Migration 018 (Android
// viewer) uses the same pattern, so we wrap the whole loop
// instead of opting in per migration. Migrations 001-017 are
// FK-consistent regardless of the pragma; turning it off for them
// is a no-op.
func applyMigrations(db *sql.DB) error {
	current, err := currentSchemaVersion(db)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	// Disable FK enforcement for the migration loop. SQLite spec:
	// foreign_keys pragma is a no-op inside a transaction, so it
	// must be set on the bare connection before any BEGIN.
	if _, err := db.Exec("PRAGMA foreign_keys = OFF"); err != nil {
		return fmt.Errorf("disable foreign keys for migration: %w", err)
	}
	defer func() {
		// Best effort: re-enable FK enforcement once the loop is
		// done, regardless of success. The caller (db.Open) also
		// sets PRAGMA foreign_keys=ON earlier, so this is mostly
		// defensive - if a migration left FK off, we restore the
		// production posture.
		_, _ = db.Exec("PRAGMA foreign_keys = ON")
	}()

	for _, name := range names {
		match := migrationFilenameRE.FindStringSubmatch(name)
		if match == nil {
			return fmt.Errorf("invalid migration filename %q (want NNN_name.sql)", name)
		}
		version, err := strconv.Atoi(match[1])
		if err != nil {
			return fmt.Errorf("parse version from %q: %w", name, err)
		}
		if version <= current {
			continue
		}
		body, err := fs.ReadFile(migrationsFS, "migrations/"+name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if err := applyOne(db, name, string(body)); err != nil {
			return err
		}
	}

	// Sanity: after all migrations the FK graph must be intact.
	// foreign_key_check returns one row per violation; an empty
	// result means everything links up cleanly.
	rows, err := db.Query("PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("foreign_key_check: %w", err)
	}
	defer rows.Close()
	var violations []string
	for rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fkid sql.NullInt64
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return fmt.Errorf("scan foreign_key_check row: %w", err)
		}
		violations = append(violations,
			fmt.Sprintf("%s rowid=%v -> %s fkid=%v",
				table, rowid.Int64, parent, fkid.Int64))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("foreign_key_check rows: %w", err)
	}
	if len(violations) > 0 {
		return fmt.Errorf("foreign_key_check after migrations failed: %v", violations)
	}
	return nil
}

// currentSchemaVersion returns MAX(version) from schema_version,
// or 0 if the table does not yet exist (first run).
func currentSchemaVersion(db *sql.DB) (int, error) {
	var name string
	err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='schema_version'`,
	).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var max sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&max); err != nil {
		return 0, err
	}
	if !max.Valid {
		return 0, nil
	}
	return int(max.Int64), nil
}

func applyOne(db *sql.DB, name, body string) error {
	body = strings.TrimSpace(body)
	if body == "" {
		return fmt.Errorf("migration %s is empty", name)
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx for %s: %w", name, err)
	}
	if _, err := tx.Exec(body); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("apply %s: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit %s: %w", name, err)
	}
	return nil
}
