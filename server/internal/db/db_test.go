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
	if version != 16 {
		t.Errorf("schema_version = %d, want 16", version)
	}
	for _, table := range []string{
		"viewers", "viewer_sessions", "admin_sessions",
		"admin_users", "platform_config", "door_events", "login_audit",
		"esp_pending_devices", "doorbell_calls",
		"viewer_hidden_events",
	} {
		var name string
		err := d.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}
	// Saison 13-02-FIX4-a: magic_link_tokens und mock_viewers
	// muessen weg sein.
	for _, gone := range []string{"magic_link_tokens", "mock_viewers", "mieter_sessions"} {
		var name string
		err := d.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, gone,
		).Scan(&name)
		if err == nil {
			t.Errorf("table %s still present after migration 006", gone)
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
	for _, v := range []int{1, 2, 3, 4, 5, 6, 7, 8, 9} {
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
	nested := filepath.Join(t.TempDir(), "deep", "state", "carvilon.db")
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

// ----- viewers Tabelle (Saison 13-02-FIX4-a, Migration 006) -----

func TestViewers_TableExists(t *testing.T) {
	d, err := Open(tempDBPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	var name string
	if err := d.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='viewers'`,
	).Scan(&name); err != nil {
		t.Fatalf("viewers table missing: %v", err)
	}
	for _, idx := range []string{
		"idx_viewers_service_port",
		"idx_viewers_type",
	} {
		var n string
		err := d.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, idx,
		).Scan(&n)
		if err != nil {
			t.Errorf("index %s missing: %v", idx, err)
		}
	}
}

func TestViewers_TypeCheck(t *testing.T) {
	d, err := Open(tempDBPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	now := int64(1747000000000)
	_, err = d.Exec(
		`INSERT INTO viewers (mac, name, service_port, type, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"0c:ea:14:42:42:42", "x", 8080, "wrong-type", now, now,
	)
	if err == nil {
		t.Fatal("insert with type='wrong-type' succeeded, CHECK constraint missing")
	}
}

func TestViewers_UniquePort(t *testing.T) {
	d, err := Open(tempDBPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	now := int64(1747000000000)
	if _, err := d.Exec(
		`INSERT INTO viewers (mac, name, service_port, type, created_at, updated_at)
		 VALUES (?, ?, ?, 'web', ?, ?)`,
		"0c:ea:14:42:42:42", "v1", 8080, now, now,
	); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, err = d.Exec(
		`INSERT INTO viewers (mac, name, service_port, type, created_at, updated_at)
		 VALUES (?, ?, ?, 'web', ?, ?)`,
		"0c:ea:14:42:42:43", "v2", 8080, now, now,
	)
	if err == nil {
		t.Fatal("duplicate port insert succeeded, unique index missing")
	}
}

// HOTFIX4: viewers.username-Spalte ist abgeschafft. Test fuer
// Uniqueness laeuft jetzt ueber den Anwendungs-Layer
// (mockmanager-Tests pruefen ErrNameInUse).
func TestViewers_UsernameColumnGone(t *testing.T) {
	d, err := Open(tempDBPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	if _, err := d.Exec(`SELECT username FROM viewers LIMIT 1`); err == nil {
		t.Error("viewers.username column still exists after migration 008")
	}
	if _, err := d.Exec(
		`INSERT INTO viewers (mac, name, service_port, type, created_at, updated_at)
		 VALUES (?, ?, ?, 'esp', ?, ?)`,
		"0c:ea:14:99:99:99", "esp1", 8082, 1, 1,
	); err != nil {
		t.Errorf("esp insert without username failed: %v", err)
	}
}

func TestViewerSessions_CascadeOnViewerDelete(t *testing.T) {
	d, err := Open(tempDBPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	now := int64(1747000000000)
	if _, err := d.Exec(
		`INSERT INTO viewers (mac, name, service_port, type, created_at, updated_at)
		 VALUES (?, ?, ?, 'web', ?, ?)`,
		"0c:ea:14:cc:cc:cc", "x", 9100, now, now,
	); err != nil {
		t.Fatalf("seed viewer: %v", err)
	}
	if _, err := d.Exec(
		`INSERT INTO viewer_sessions (session_id, viewer_mac, created_at, last_seen, expires_at)
		 VALUES (?, ?, ?, ?, ?)`,
		"sess-x", "0c:ea:14:cc:cc:cc", now, now, now,
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := d.Exec(`DELETE FROM viewers WHERE mac = ?`, "0c:ea:14:cc:cc:cc"); err != nil {
		t.Fatalf("delete viewer: %v", err)
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM viewer_sessions WHERE viewer_mac = ?`,
		"0c:ea:14:cc:cc:cc").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("viewer_sessions cascade missed; remaining = %d", n)
	}
}

func TestDoorEvents_FKAndCascade(t *testing.T) {
	d, err := Open(tempDBPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	now := int64(1747000000)
	_, err = d.Exec(
		`INSERT INTO door_events (viewer_mac, event_type, occurred_at) VALUES (?, ?, ?)`,
		"0c:ea:14:11:22:33", "doorbell_start", now,
	)
	if err == nil {
		t.Fatal("insert without viewer succeeded, FK not enforced")
	}
	if _, err := d.Exec(
		`INSERT INTO viewers (mac, name, service_port, type, created_at, updated_at)
		 VALUES (?, ?, ?, 'web', ?, ?)`,
		"0c:ea:14:11:22:33", "y", 9200, now*1000, now*1000,
	); err != nil {
		t.Fatalf("seed viewer: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := d.Exec(
			`INSERT INTO door_events (viewer_mac, event_type, occurred_at) VALUES (?, ?, ?)`,
			"0c:ea:14:11:22:33", "doorbell_start", now+int64(i),
		); err != nil {
			t.Fatalf("insert event: %v", err)
		}
	}
	if _, err := d.Exec(`DELETE FROM viewers WHERE mac = ?`, "0c:ea:14:11:22:33"); err != nil {
		t.Fatalf("delete viewer: %v", err)
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM door_events WHERE viewer_mac = ?`,
		"0c:ea:14:11:22:33").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("door_events cascade missed; remaining = %d", n)
	}
}

func TestLoginAudit_TableShape(t *testing.T) {
	d, err := Open(tempDBPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	now := int64(1747000000000)
	if _, err := d.Exec(
		`INSERT INTO login_audit (timestamp, realm, username, ip, user_agent, outcome)
		 VALUES (?, 'viewer', 'alice', '1.1.1.1', 'curl', 'success')`,
		now,
	); err != nil {
		t.Fatalf("insert audit: %v", err)
	}
	_, err = d.Exec(
		`INSERT INTO login_audit (timestamp, realm, outcome)
		 VALUES (?, 'wrong-realm', 'success')`,
		now,
	)
	if err == nil {
		t.Fatal("realm-CHECK didn't reject bad realm")
	}
	_, err = d.Exec(
		`INSERT INTO login_audit (timestamp, realm, outcome)
		 VALUES (?, 'viewer', 'wrong-outcome')`,
		now,
	)
	if err == nil {
		t.Fatal("outcome-CHECK didn't reject bad outcome")
	}
}
