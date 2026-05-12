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
	if version != 4 {
		t.Errorf("schema_version = %d, want 4", version)
	}
	for _, table := range []string{
		"magic_link_tokens", "mieter_sessions", "admin_sessions", "mock_viewers",
		"admin_users", "platform_config",
	} {
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
	for _, v := range []int{1, 2, 3, 4} {
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

func TestMigration003_Applied(t *testing.T) {
	d, err := Open(tempDBPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	for _, table := range []string{"admin_users", "platform_config"} {
		var name string
		err := d.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}
}

func TestMigration003_AdminUsersPrimaryKey(t *testing.T) {
	d, err := Open(tempDBPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	now := int64(1747000000000)
	if _, err := d.Exec(
		`INSERT INTO admin_users (username, password_hash, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		"admin", "hash1", now, now,
	); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if _, err := d.Exec(
		`INSERT INTO admin_users (username, password_hash, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		"admin", "hash2", now, now,
	); err == nil {
		t.Fatal("duplicate username insert succeeded, want primary-key error")
	}
}

func TestMigration003_PlatformConfigPrimaryKey(t *testing.T) {
	d, err := Open(tempDBPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	now := int64(1747000000000)
	if _, err := d.Exec(
		`INSERT INTO platform_config (key, value, updated_at) VALUES (?, ?, ?)`,
		"ua_api_base_url", "https://192.168.1.1:12445", now,
	); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if _, err := d.Exec(
		`INSERT INTO platform_config (key, value, updated_at) VALUES (?, ?, ?)`,
		"ua_api_base_url", "https://other", now,
	); err == nil {
		t.Fatal("duplicate key insert succeeded, want primary-key error")
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
	for _, idx := range []string{"idx_mock_viewers_port"} {
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

// ----- Migration 004 -----

func TestMigration004_DropsUAUserIDFromMockViewers(t *testing.T) {
	d, err := Open(tempDBPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	// ua_user_id column must be gone.
	_, err = d.Exec(`SELECT ua_user_id FROM mock_viewers LIMIT 1`)
	if err == nil {
		t.Error("ua_user_id column still present in mock_viewers")
	}
	// Old index gone too.
	var name string
	err = d.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_mock_viewers_ua_user'`,
	).Scan(&name)
	if err == nil {
		t.Error("idx_mock_viewers_ua_user still exists; migration 004 should have dropped it")
	}
}

func TestMigration004_MieterSessionsHasMockFK(t *testing.T) {
	d, err := Open(tempDBPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	now := int64(1747000000000)
	// Insert without an existing mock must fail because of FK.
	_, err = d.Exec(
		`INSERT INTO mieter_sessions (session_id, mock_mac, created_at, last_seen, expires_at)
		 VALUES (?, ?, ?, ?, ?)`,
		"sess-1", "0c:ea:14:00:00:00", now, now, now,
	)
	if err == nil {
		t.Fatal("inserted mieter session without matching mock_viewer; FK not enforced")
	}
	// Now create the mock, insert should succeed.
	if _, err := d.Exec(
		`INSERT INTO mock_viewers (mac, name, service_port, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"0c:ea:14:00:00:00", "x", 9000, now, now,
	); err != nil {
		t.Fatalf("insert mock: %v", err)
	}
	if _, err := d.Exec(
		`INSERT INTO mieter_sessions (session_id, mock_mac, created_at, last_seen, expires_at)
		 VALUES (?, ?, ?, ?, ?)`,
		"sess-1", "0c:ea:14:00:00:00", now, now, now,
	); err != nil {
		t.Fatalf("insert mieter session with valid FK: %v", err)
	}
}

func TestMigration004_CascadeDeletesSessionOnMockRemoval(t *testing.T) {
	d, err := Open(tempDBPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	now := int64(1747000000000)
	if _, err := d.Exec(
		`INSERT INTO mock_viewers (mac, name, service_port, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"0c:ea:14:cc:dd:ee", "x", 9001, now, now,
	); err != nil {
		t.Fatalf("insert mock: %v", err)
	}
	if _, err := d.Exec(
		`INSERT INTO mieter_sessions (session_id, mock_mac, created_at, last_seen, expires_at) VALUES (?, ?, ?, ?, ?)`,
		"sess-cascade", "0c:ea:14:cc:dd:ee", now, now, now,
	); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err := d.Exec(
		`INSERT INTO magic_link_tokens (token, mock_mac, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		"tok-cascade", "0c:ea:14:cc:dd:ee", now, now,
	); err != nil {
		t.Fatalf("insert token: %v", err)
	}
	if _, err := d.Exec(`DELETE FROM mock_viewers WHERE mac = ?`, "0c:ea:14:cc:dd:ee"); err != nil {
		t.Fatalf("delete mock: %v", err)
	}
	for _, q := range []struct {
		label string
		stmt  string
	}{
		{"mieter_sessions", `SELECT COUNT(*) FROM mieter_sessions WHERE mock_mac = ?`},
		{"magic_link_tokens", `SELECT COUNT(*) FROM magic_link_tokens WHERE mock_mac = ?`},
	} {
		var n int
		if err := d.QueryRow(q.stmt, "0c:ea:14:cc:dd:ee").Scan(&n); err != nil {
			t.Fatalf("count %s: %v", q.label, err)
		}
		if n != 0 {
			t.Errorf("%s row count after cascade = %d, want 0", q.label, n)
		}
	}
}

func TestMigration004_AdminSessionsTableExists(t *testing.T) {
	d, err := Open(tempDBPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	var name string
	if err := d.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='admin_sessions'`,
	).Scan(&name); err != nil {
		t.Fatalf("admin_sessions table missing: %v", err)
	}
	// The old shared sessions table must be gone.
	err = d.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='sessions'`,
	).Scan(&name)
	if err == nil {
		t.Error("shared sessions table still present; migration 004 should have replaced it with mieter_sessions and admin_sessions")
	}
}
