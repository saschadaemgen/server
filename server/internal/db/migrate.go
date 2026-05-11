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
