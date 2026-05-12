package db

import (
	"os"
	"path/filepath"
	"testing"
)

func tempDBPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "test.db")
}

func TestOpen_CreatesDBIfNotExists(t *testing.T) {
	path := tempDBPath(t)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("test setup: db file already exists at %s", path)
	}
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("db file not created on Open: %v", err)
	}
	if d.Path() != path {
		t.Errorf("Path() = %q, want %q", d.Path(), path)
	}
}

func TestOpen_AppliesMigrations(t *testing.T) {
	d, err := Open(tempDBPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	var version int
	if err := d.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("query schema_version: %v", err)
	}
	if version != 1 {
		t.Errorf("schema_version = %d, want 1", version)
	}
	for _, table := range []string{"magic_link_tokens", "sessions"} {
		var name string
		err := d.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}
}

func TestOpen_IdempotentReapply(t *testing.T) {
	path := tempDBPath(t)
	d, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer d.Close()
	var count int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM schema_version WHERE version = 1`,
	).Scan(&count); err != nil {
		t.Fatalf("query schema_version: %v", err)
	}
	if count != 1 {
		t.Errorf("schema_version v=1 row count = %d, want 1 (idempotency broken)", count)
	}
}

func TestOpen_PragmasSet(t *testing.T) {
	d, err := Open(tempDBPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	var journal string
	if err := d.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if journal != "wal" {
		t.Errorf("journal_mode = %q, want %q", journal, "wal")
	}
	var fk int
	if err := d.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("query foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1 (on)", fk)
	}
}

func TestOpen_EmptyPathRejected(t *testing.T) {
	_, err := Open("")
	if err == nil {
		t.Fatal("Open with empty path returned nil error")
	}
}

func TestOpen_CreatesParentDirIfMissing(t *testing.T) {
	nested := filepath.Join(t.TempDir(), "deep", "state", "unifix.db")
	if _, err := os.Stat(filepath.Dir(nested)); !os.IsNotExist(err) {
		t.Fatalf("test setup: parent dir already exists")
	}
	d, err := Open(nested)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	if _, err := os.Stat(filepath.Dir(nested)); err != nil {
		t.Errorf("parent dir not created: %v", err)
	}
	if _, err := os.Stat(nested); err != nil {
		t.Errorf("db file not created: %v", err)
	}
}
