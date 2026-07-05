package httpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"carvilon.local/server/internal/designerstore"
	"carvilon.local/server/internal/readerstore"
)

func seedReader(t *testing.T, env *testEnv, det readerstore.Detected) {
	t.Helper()
	ds := designerstore.New(env.d.DB)
	if err := env.readerStore.Sync(context.Background(), []readerstore.Detected{det}, ds.EnsureReaderGraph); err != nil {
		t.Fatalf("seed reader: %v", err)
	}
}

// TestAdminNFC_EmptyState: with no readers the page renders a clean
// empty state and the NFC nav link is present.
func TestAdminNFC_EmptyState(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	body := getBody(t, env, "/a/nfc")
	if !strings.Contains(body, "Kein Leser erkannt") {
		t.Errorf("empty NFC page missing the empty state")
	}
	if !strings.Contains(body, `href="/a/nfc"`) {
		t.Errorf("NFC nav link missing from the topbar")
	}
}

// TestAdminNFC_ListsReaderWithJumpAndTag: a registered reader shows with
// its online status, the editor-jump link into its System/Reader graph,
// and (after a tag read) its last-seen UID.
func TestAdminNFC_ListsReaderWithJumpAndTag(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	seedReader(t, env, readerstore.Detected{
		ID: "nfc:i2c-1", Kind: "nfc", Model: "PN532", Firmware: "1.6", Bus: "i2c-1", Name: "PN532 · i2c-1",
	})

	body := getBody(t, env, "/a/nfc")
	if !strings.Contains(body, "PN532 · i2c-1") {
		t.Errorf("reader name missing from page")
	}
	if !strings.Contains(body, "online") {
		t.Errorf("online status missing")
	}
	if !strings.Contains(body, "/a/designer?g=") {
		t.Errorf("editor jump link missing")
	}
	if !strings.Contains(body, "noch keins") {
		t.Errorf("no-tag placeholder missing before any read")
	}

	if err := env.readerStore.NoteTag(context.Background(), "nfc:i2c-1", "04:A3:1B:2C"); err != nil {
		t.Fatalf("NoteTag: %v", err)
	}
	body = getBody(t, env, "/a/nfc")
	if !strings.Contains(body, "04:A3:1B:2C") {
		t.Errorf("last-seen tag UID missing from page after a read")
	}
}

// TestAdminNFC_OfflineReaderShown: a reader that dropped out stays on the
// page as offline (never silently gone).
func TestAdminNFC_OfflineReaderShown(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	seedReader(t, env, readerstore.Detected{ID: "nfc:i2c-1", Kind: "nfc", Model: "PN532", Bus: "i2c-1", Name: "PN532 · i2c-1"})
	// Hardware gone.
	ds := designerstore.New(env.d.DB)
	if err := env.readerStore.Sync(context.Background(), nil, ds.EnsureReaderGraph); err != nil {
		t.Fatalf("Sync empty: %v", err)
	}
	body := getBody(t, env, "/a/nfc")
	if !strings.Contains(body, "PN532 · i2c-1") {
		t.Errorf("gone reader disappeared from the page")
	}
	if !strings.Contains(body, "offline") {
		t.Errorf("offline status missing")
	}
}

// TestAdminNFC_JSONSignature: the poll endpoint returns JSON and its
// signature changes when a tag is read (drives the page auto-refresh).
func TestAdminNFC_JSONSignature(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	seedReader(t, env, readerstore.Detected{ID: "nfc:i2c-1", Kind: "nfc", Model: "PN532", Bus: "i2c-1", Name: "PN532 · i2c-1"})

	sig1, n1 := nfcJSON(t, env)
	if n1 != 1 {
		t.Fatalf("json readers = %d, want 1", n1)
	}
	if err := env.readerStore.NoteTag(context.Background(), "nfc:i2c-1", "AA:BB:CC:DD"); err != nil {
		t.Fatalf("NoteTag: %v", err)
	}
	sig2, _ := nfcJSON(t, env)
	if sig1 == sig2 {
		t.Errorf("signature unchanged after a tag read (%q)", sig1)
	}
}

func nfcJSON(t *testing.T, env *testEnv) (sig string, readers int) {
	t.Helper()
	resp, err := env.client.Get(env.ts.URL + "/a/nfc.json")
	if err != nil {
		t.Fatalf("GET /a/nfc.json: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	var payload struct {
		Readers int    `json:"readers"`
		Online  int    `json:"online"`
		Sig     string `json:"sig"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode nfc.json: %v", err)
	}
	return payload.Sig, payload.Readers
}
