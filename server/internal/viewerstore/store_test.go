package viewerstore_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"carvilon.local/server/internal/db"
	"carvilon.local/server/internal/viewerstore"
)

// openTempDB opens a fresh sqlite file in t.TempDir and runs the
// migrations. The wrapper exists so each test case starts from a
// clean schema without touching any production state.
func openTempDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d.DB
}

// TestInsert_MinimalESP covers the CLI's call shape: an ESP row
// with only the columns the cli/esp.go adopt path was setting
// before the refactor. Everything else relies on the spec's zero
// values translating to SQL NULL (or the column DDL default).
func TestInsert_MinimalESP(t *testing.T) {
	d := openTempDB(t)
	ctx := context.Background()

	spec := viewerstore.InsertSpec{
		MAC:               "0c:ea:14:aa:bb:cc",
		Name:              "ESP-A",
		ServicePort:       8100,
		Type:              "esp",
		ESPTokenHash:      "deadbeef",
		PairedIntercomMAC: "28:70:4e:31:e2:9c",
		LinkedUAUserID:    "ua-user-42",
	}
	if err := viewerstore.Insert(ctx, d, spec, 1747000000000); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	var (
		typ        string
		hash       string
		port       int64
		paired     string
		linkedUser sql.NullString
		espModel   sql.NullString
		espFW      sql.NullString
		streamProf sql.NullString
		idleMode   sql.NullString
		autoSec    sql.NullInt64
		espPending int64
		created    int64
		updated    int64
	)
	err := d.QueryRow(`SELECT type, esp_token_hash, service_port,
	                          paired_intercom_mac, linked_ua_user_id,
	                          esp_model, esp_fw_version, stream_profile,
	                          idle_view_mode, auto_screensaver_seconds,
	                          esp_pending, created_at, updated_at
	                     FROM viewers WHERE mac = ?`, spec.MAC,
	).Scan(&typ, &hash, &port, &paired, &linkedUser,
		&espModel, &espFW, &streamProf, &idleMode, &autoSec,
		&espPending, &created, &updated)
	if err != nil {
		t.Fatalf("probe row: %v", err)
	}

	if typ != "esp" {
		t.Errorf("type = %q, want esp", typ)
	}
	if hash != "deadbeef" {
		t.Errorf("hash = %q", hash)
	}
	if port != 8100 {
		t.Errorf("port = %d", port)
	}
	if paired != "28:70:4e:31:e2:9c" {
		t.Errorf("paired = %q", paired)
	}
	if !linkedUser.Valid || linkedUser.String != "ua-user-42" {
		t.Errorf("linked = %+v", linkedUser)
	}
	// Unset fields land as SQL NULL.
	if espModel.Valid {
		t.Errorf("esp_model should be NULL, got %q", espModel.String)
	}
	if espFW.Valid {
		t.Errorf("esp_fw_version should be NULL, got %q", espFW.String)
	}
	if streamProf.Valid {
		t.Errorf("stream_profile should be NULL, got %q", streamProf.String)
	}
	if idleMode.Valid {
		t.Errorf("idle_view_mode should be NULL, got %q", idleMode.String)
	}
	if autoSec.Valid {
		t.Errorf("auto_screensaver_seconds should be NULL, got %d", autoSec.Int64)
	}
	// esp_pending falls back to the DDL default 0.
	if espPending != 0 {
		t.Errorf("esp_pending = %d, want 0 (DDL default)", espPending)
	}
	if created != 1747000000000 {
		t.Errorf("created_at = %d", created)
	}
	if updated != 1747000000000 {
		t.Errorf("updated_at = %d", updated)
	}
}

// TestInsert_FullManagerShape mirrors the call shape the
// viewermanager uses when an admin creates a viewer through the
// web UI: every spec field carries data, the empty-string and
// nil-pointer branches stay untested. This pins the column order
// + nullable mapping so a future schema change cannot drift
// silently.
func TestInsert_FullManagerShape(t *testing.T) {
	d := openTempDB(t)
	ctx := context.Background()

	autoSec := 300
	spec := viewerstore.InsertSpec{
		MAC:                    "0c:ea:14:11:22:33",
		Name:                   "Wohnung 4",
		ServicePort:            8123,
		Type:                   "web",
		LinkedUAUserID:         "ua-user-7",
		ESPModel:               "esp32-p4",
		ESPFwVersion:           "v0.3.0",
		ESPTokenHash:           "feedface",
		PairedIntercomMAC:      "  28:70:4E:31:E2:9C  ",
		StreamProfile:          "  intercom_web  ",
		IdleViewMode:           "  livestream  ",
		AutoScreensaverSeconds: &autoSec,
	}
	if err := viewerstore.Insert(ctx, d, spec, 1747000000000); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	var (
		paired     string
		streamProf string
		idleMode   string
		autoOut    int64
		espModel   string
		espFW      string
	)
	err := d.QueryRow(`SELECT paired_intercom_mac, stream_profile,
	                          idle_view_mode, auto_screensaver_seconds,
	                          esp_model, esp_fw_version
	                     FROM viewers WHERE mac = ?`, spec.MAC,
	).Scan(&paired, &streamProf, &idleMode, &autoOut, &espModel, &espFW)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if paired != "28:70:4e:31:e2:9c" {
		t.Errorf("paired = %q (expected normalised lowercase trim)", paired)
	}
	if streamProf != "intercom_web" {
		t.Errorf("stream_profile = %q (expected trimmed)", streamProf)
	}
	if idleMode != "livestream" {
		t.Errorf("idle_view_mode = %q (expected trimmed)", idleMode)
	}
	if autoOut != 300 {
		t.Errorf("auto_screensaver_seconds = %d", autoOut)
	}
	if espModel != "esp32-p4" {
		t.Errorf("esp_model = %q", espModel)
	}
	if espFW != "v0.3.0" {
		t.Errorf("esp_fw_version = %q", espFW)
	}
}

// TestInsert_AutoScreensaverZeroBecomesNull asserts the
// "0 == feature off == NULL" convention so the resolver's defaults
// keep working for spec.AutoScreensaverSeconds == &0.
func TestInsert_AutoScreensaverZeroBecomesNull(t *testing.T) {
	d := openTempDB(t)
	zero := 0
	spec := viewerstore.InsertSpec{
		MAC:                    "0c:ea:14:00:00:01",
		Name:                   "Z",
		ServicePort:            8100,
		Type:                   "web",
		AutoScreensaverSeconds: &zero,
	}
	if err := viewerstore.Insert(context.Background(), d, spec, 1); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	var n sql.NullInt64
	if err := d.QueryRow(
		`SELECT auto_screensaver_seconds FROM viewers WHERE mac = ?`, spec.MAC,
	).Scan(&n); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if n.Valid {
		t.Errorf("auto_screensaver_seconds = %d, want NULL for zero pointer", n.Int64)
	}
}

// TestInsert_DuplicateMACErrors confirms the PK constraint surfaces
// as a normal error (the caller's responsibility to map it; the
// store layer just propagates).
func TestInsert_DuplicateMACErrors(t *testing.T) {
	d := openTempDB(t)
	ctx := context.Background()
	spec := viewerstore.InsertSpec{
		MAC: "0c:ea:14:dd:ee:ff", Name: "A", ServicePort: 8100, Type: "web",
	}
	if err := viewerstore.Insert(ctx, d, spec, 1); err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	spec.Name = "B"
	if err := viewerstore.Insert(ctx, d, spec, 2); err == nil {
		t.Fatal("expected error on duplicate MAC, got nil")
	}
}

// TestNextFreeServicePort_EmptyTable returns the start constant.
func TestNextFreeServicePort_EmptyTable(t *testing.T) {
	d := openTempDB(t)
	port, err := viewerstore.NextFreeServicePort(context.Background(), d)
	if err != nil {
		t.Fatalf("NextFreeServicePort: %v", err)
	}
	if port != viewerstore.ServicePortStart {
		t.Errorf("port = %d, want %d", port, viewerstore.ServicePortStart)
	}
}

// TestNextFreeServicePort_AfterSeed returns max+1.
func TestNextFreeServicePort_AfterSeed(t *testing.T) {
	d := openTempDB(t)
	ctx := context.Background()
	if err := viewerstore.Insert(ctx, d, viewerstore.InsertSpec{
		MAC: "0c:ea:14:00:00:01", Name: "S1", ServicePort: 8123, Type: "web",
	}, 1); err != nil {
		t.Fatalf("seed: %v", err)
	}
	port, err := viewerstore.NextFreeServicePort(ctx, d)
	if err != nil {
		t.Fatalf("NextFreeServicePort: %v", err)
	}
	if port != 8124 {
		t.Errorf("port = %d, want 8124 (8123 + 1)", port)
	}
}

// TestNextFreeServicePort_BelowStart skips ports below
// ServicePortStart even when present in the table (defensive: a
// pre-seasoned dev environment with a port=80 row should still
// get 8100 for the next auto-allocation).
func TestNextFreeServicePort_BelowStart(t *testing.T) {
	d := openTempDB(t)
	ctx := context.Background()
	if err := viewerstore.Insert(ctx, d, viewerstore.InsertSpec{
		MAC: "0c:ea:14:00:00:02", Name: "S2", ServicePort: 80, Type: "web",
	}, 1); err != nil {
		t.Fatalf("seed: %v", err)
	}
	port, err := viewerstore.NextFreeServicePort(ctx, d)
	if err != nil {
		t.Fatalf("NextFreeServicePort: %v", err)
	}
	if port != viewerstore.ServicePortStart {
		t.Errorf("port = %d, want %d", port, viewerstore.ServicePortStart)
	}
}
