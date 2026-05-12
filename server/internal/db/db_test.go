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
	if version != 2 {
		t.Errorf("schema_version = %d, want 2", version)
	}
	for _, table := range []string{"magic_link_tokens", "sessions", "mock_viewers"} {
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
	for _, v := range []int{1, 2} {
		var count int
		if err := d.QueryRow(
			`SELECT COUNT(*) FROM schema_version WHERE version = ?`, v,
		).Scan(&count); err != nil {
			t.Fatalf("query schema_version v=%d: %v", v, err)
		}
		if count != 1 {
			t.Errorf("schema_version v=%d row count = %d, want 1 (idempotency broken)", v, count)
		}
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

func TestMigration002_Applied(t *testing.T) {
	d, err := Open(tempDBPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	var max int
	if err := d.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&max); err != nil {
		t.Fatalf("query: %v", err)
	}
	if max < 2 {
		t.Errorf("MAX(version) = %d, want >= 2", max)
	}
}

func TestMigration002_MockViewersTableExists(t *testing.T) {
	d, err := Open(tempDBPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	var name string
	err = d.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='mock_viewers'`,
	).Scan(&name)
	if err != nil {
		t.Fatalf("mock_viewers table missing: %v", err)
	}
	for _, idx := range []string{"idx_mock_viewers_ua_user", "idx_mock_viewers_port"} {
		var idxName string
		err := d.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, idx,
		).Scan(&idxName)
		if err != nil {
			t.Errorf("index %s missing: %v", idx, err)
		}
	}
}

func TestMigration002_UniquePortConstraint(t *testing.T) {
	d, err := Open(tempDBPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	now := int64(1747000000000)
	if _, err := d.Exec(
		`INSERT INTO mock_viewers (mac, name, service_port, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"0c:ea:14:42:42:42", "viewer-1", 8080, now, now,
	); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, err = d.Exec(
		`INSERT INTO mock_viewers (mac, name, service_port, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"0c:ea:14:42:42:43", "viewer-2", 8080, now, now,
	)
	if err == nil {
		t.Fatal("second insert with same port succeeded, want unique-constraint error")
	}
}

func TestMigration002_UniqueMACConstraint(t *testing.T) {
	d, err := Open(tempDBPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	now := int64(1747000000000)
	if _, err := d.Exec(
		`INSERT INTO mock_viewers (mac, name, service_port, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"0c:ea:14:42:42:42", "viewer-1", 8080, now, now,
	); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, err = d.Exec(
		`INSERT INTO mock_viewers (mac, name, service_port, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"0c:ea:14:42:42:42", "viewer-dup", 8081, now, now,
	)
	if err == nil {
		t.Fatal("second insert with same MAC succeeded, want primary-key error")
	}
}
