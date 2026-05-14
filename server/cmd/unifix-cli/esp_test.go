package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"unifix.local/server/internal/auth/esptoken"
	"unifix.local/server/internal/db"
)

// runAdoptForTest runs the adopt subcommand against a fresh
// SQLite DB and returns the captured stdout + the open DB so
// the test can probe what landed in the viewers table.
func runAdoptForTest(t *testing.T, args ...string) (string, *db.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	full := append([]string{"--db", dbPath}, args...)
	var out bytes.Buffer
	if err := runESPAdopt(full, &out); err != nil {
		t.Fatalf("runESPAdopt: %v\n--- stdout ---\n%s", err, out.String())
	}
	return out.String(), d
}

func TestESPAdopt_PersistsRowAndPrintsToken(t *testing.T) {
	stdout, d := runAdoptForTest(t,
		"--mac", "0c:ea:14:aa:bb:cc",
		"--name", "Produktausgabe-ESP",
		"--intercom", "28:70:4e:31:e2:9c",
		"--mieter", "ua-user-42",
	)

	// Token line is the last non-empty stdout line.
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	tok := strings.TrimSpace(lines[len(lines)-1])
	if tok == "" {
		t.Fatalf("no token printed; stdout=%s", stdout)
	}
	if !strings.Contains(stdout, "Produktausgabe-ESP") {
		t.Errorf("name not echoed; stdout=%s", stdout)
	}

	// Row probed straight from DB.
	var (
		name           string
		typ            string
		hash           string
		pairedIntercom string
		linkedUser     string
		port           int64
	)
	err := d.QueryRow(
		`SELECT name, type, esp_token_hash,
		        COALESCE(paired_intercom_mac, ''),
		        COALESCE(linked_ua_user_id, ''),
		        service_port
		   FROM viewers WHERE mac = ?`,
		"0c:ea:14:aa:bb:cc",
	).Scan(&name, &typ, &hash, &pairedIntercom, &linkedUser, &port)
	if err != nil {
		t.Fatalf("probe row: %v", err)
	}
	if name != "Produktausgabe-ESP" {
		t.Errorf("name = %q", name)
	}
	if typ != "esp" {
		t.Errorf("type = %q, want esp", typ)
	}
	if pairedIntercom != "28:70:4e:31:e2:9c" {
		t.Errorf("paired_intercom = %q", pairedIntercom)
	}
	if linkedUser != "ua-user-42" {
		t.Errorf("linked_ua_user_id = %q", linkedUser)
	}
	if port < servicePortStart {
		t.Errorf("port = %d, want >= %d", port, servicePortStart)
	}
	if !esptoken.Verify(tok, hash) {
		t.Errorf("printed token does not match stored hash")
	}
}

func TestESPAdopt_AllocatesNextFreePort(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	// Pre-seed a viewer at port 8123 so the CLI must skip past it.
	_, err = d.Exec(
		`INSERT INTO viewers (mac, name, service_port, type, created_at, updated_at)
		 VALUES ('0c:ea:14:00:00:01', 'Existing', 8123, 'web', 0, 0)`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	var out bytes.Buffer
	if err := runESPAdopt([]string{
		"--db", dbPath,
		"--mac", "0c:ea:14:aa:bb:cc",
		"--name", "ESP-Two",
	}, &out); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	var port int64
	_ = d.QueryRow(`SELECT service_port FROM viewers WHERE mac = ?`,
		"0c:ea:14:aa:bb:cc").Scan(&port)
	if port != 8124 {
		t.Errorf("port = %d, want 8124 (next after 8123)", port)
	}
}

func TestESPAdopt_RejectsBadMAC(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	d, _ := db.Open(dbPath)
	defer d.Close()
	var out bytes.Buffer
	err := runESPAdopt([]string{
		"--db", dbPath,
		"--mac", "AA-BB-CC-DD-EE-FF", // dashes, not colons
		"--name", "X",
	}, &out)
	if err == nil {
		t.Fatal("expected error for bad MAC form")
	}
	if !strings.Contains(err.Error(), "lowercase colon-form") {
		t.Errorf("err = %v, want hint about colon-form", err)
	}
}

func TestESPAdopt_RejectsMissingMAC(t *testing.T) {
	var out bytes.Buffer
	err := runESPAdopt([]string{"--name", "X"}, &out)
	if err == nil {
		t.Fatal("expected error for missing --mac")
	}
}

func TestESPAdopt_RejectsMissingName(t *testing.T) {
	var out bytes.Buffer
	err := runESPAdopt([]string{"--mac", "0c:ea:14:aa:bb:cc"}, &out)
	if err == nil {
		t.Fatal("expected error for missing --name")
	}
}

func TestESPAdopt_DuplicateMACErrors(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	d, _ := db.Open(dbPath)
	defer d.Close()

	args := []string{"--db", dbPath, "--mac", "0c:ea:14:aa:bb:cc", "--name", "First"}
	var out bytes.Buffer
	if err := runESPAdopt(args, &out); err != nil {
		t.Fatalf("first adopt: %v", err)
	}
	args[5] = "Second" // change name to dodge name-uniqueness if enforced
	out.Reset()
	if err := runESPAdopt(args, &out); err == nil {
		t.Fatal("expected error for duplicate MAC, got nil")
	}
}
