package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"carvilon.local/server/internal/readerstore"
	"carvilon.local/server/internal/uaapi"
)

// The local reader registry as the Device Center's third source
// ("RPi"). These tests cover what moved here from the retired
// standalone NFC page: readers listed with status, rename, offline
// persistence - now inside /a/devices.

func seedReader(t *testing.T, env *testEnv, det readerstore.Detected) {
	t.Helper()
	if err := env.readerStore.Sync(context.Background(), []readerstore.Detected{det}); err != nil {
		t.Fatalf("seed reader: %v", err)
	}
}

var readerA = readerstore.Detected{
	ID: "nfc:i2c-1", Kind: "nfc", Model: "PN532", Firmware: "1.6", Bus: "i2c-1", Name: "RPi-NFC-PN532 (I2C-1)",
}

// A registered reader renders the Device Center even with UA and
// Protect both off: no gate card, a Readers group, source "RPi" as a
// real facet, the reader's controls in its actions template - and not
// a single UDM call.
func TestAdminUA_ReaderRowWithUAOff(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	stub := newUAStub(t)
	env.srv.SetUAClient(uaapi.New(uaapi.Options{BaseURL: stub.ts.URL, Token: "t"}))
	// KeyUAEnabled left unset -> uaEnabled=false; Protect unset too.
	seedReader(t, env, readerA)

	body := getBody(t, env, "/a/devices")
	if strings.Contains(body, `class="dc-gate`) {
		t.Errorf("gate card shown although a local reader fills the page")
	}
	for _, want := range []string{
		"RPi-NFC-PN532 (I2C-1)",     // auto-name
		">Readers<",                 // group heading
		`data-source="rpi"`,         // row source key
		`data-dc-value="rpi"`,       // source facet
		">RPi<",                     // source facet label
		"PN532",                     // model column + facet
		`data-kind="rpi-reader"`,    // panel behaviour switch
		"Reader controls",           // actions template
		`action="/a/devices/readers/name"`,
		`href="/a/designer"`,        // editor jump
		"only devices from other sources are shown", // UA notice banner
	} {
		if !strings.Contains(body, want) {
			t.Errorf("reader overview missing %q", want)
		}
	}
	if h := atomic.LoadInt32(&stub.hits); h != 0 {
		t.Errorf("UDM was called %d times while UA disabled, want 0", h)
	}
}

// The standalone NFC page is gone: nav carries no /a/nfc link and the
// old routes answer 404.
func TestAdminUA_NFCPageGone(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	seedReader(t, env, readerA)

	body := getBody(t, env, "/a/devices")
	if strings.Contains(body, `href="/a/nfc"`) {
		t.Errorf("NFC nav link still in the topbar")
	}
	for _, path := range []string{"/a/nfc", "/a/nfc.json"} {
		resp, err := env.client.Get(env.ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404", path, resp.StatusCode)
		}
	}
}

// With UA on, the local reader shares the Readers category with the UA
// readers and both source facets carry their own counts.
func TestAdminUA_ReaderRowAlongsideUA(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	wireUA(t, env, newUAStub(t))
	seedReader(t, env, readerA)

	body := getBody(t, env, "/a/devices")
	for _, want := range []string{
		"Leser Eingang",          // UA reader
		"RPi-NFC-PN532 (I2C-1)",  // local reader
		`data-dc-value="unifi"`, `data-dc-value="rpi"`, // both source facets
	} {
		if !strings.Contains(body, want) {
			t.Errorf("mixed overview missing %q", want)
		}
	}
	// Exactly one Readers group heading covering both readers (the
	// category facet label matches ">Readers<" too, so count the
	// group-label markup specifically).
	if n := strings.Count(body, `dc-group-label">Readers<`); n != 1 {
		t.Errorf("Readers group headings = %d, want 1", n)
	}
}

// Renaming from the panel: custom name overrides the auto-name,
// clearing reverts, the flash banner reports the outcome, and the
// submitted text is never reflected into the redirect.
func TestAdminUA_ReaderRename(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	seedReader(t, env, readerA)

	resp, err := env.client.PostForm(env.ts.URL+"/a/devices/readers/name",
		url.Values{"id": {"nfc:i2c-1"}, "name": {"Front door"}})
	if err != nil {
		t.Fatalf("POST rename: %v", err)
	}
	resp.Body.Close()
	// The test client never auto-follows, so the 303 + Location are
	// asserted directly.
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || loc != "/a/devices?flash=renamed" {
		t.Errorf("redirect = %d %q, want 303 /a/devices?flash=renamed", resp.StatusCode, loc)
	}

	body := getBody(t, env, "/a/devices?flash=renamed")
	if !strings.Contains(body, "Front door") {
		t.Errorf("custom name missing from page")
	}
	if !strings.Contains(body, "RPi-NFC-PN532 (I2C-1)") {
		t.Errorf("auto-name hint missing after rename")
	}
	if !strings.Contains(body, "Reader name saved.") {
		t.Errorf("flash banner missing")
	}

	// Clearing reverts to the auto-name.
	resp, err = env.client.PostForm(env.ts.URL+"/a/devices/readers/name",
		url.Values{"id": {"nfc:i2c-1"}, "name": {""}})
	if err != nil {
		t.Fatalf("POST clear: %v", err)
	}
	resp.Body.Close()
	r, err := env.readerStore.Get(context.Background(), "nfc:i2c-1")
	if err != nil || r.CustomName != "" {
		t.Fatalf("clear did not revert: r=%+v err=%v", r, err)
	}

	// Unknown reader -> the not-found flash code, never free text.
	resp, err = env.client.PostForm(env.ts.URL+"/a/devices/readers/name",
		url.Values{"id": {"nfc:i2c-9"}, "name": {"x"}})
	if err != nil {
		t.Fatalf("POST unknown: %v", err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "/a/devices?flash=err-notfd" {
		t.Errorf("unknown-reader redirect = %q, want /a/devices?flash=err-notfd", loc)
	}
}

// A reader that dropped out stays on the page as offline (never
// silently gone).
func TestAdminUA_ReaderOfflineShown(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	seedReader(t, env, readerA)
	if err := env.readerStore.Sync(context.Background(), nil); err != nil {
		t.Fatalf("Sync empty: %v", err)
	}
	body := getBody(t, env, "/a/devices")
	if !strings.Contains(body, "RPi-NFC-PN532 (I2C-1)") {
		t.Errorf("gone reader disappeared from the page")
	}
	if !strings.Contains(body, `data-status="offline"`) {
		t.Errorf("offline status missing")
	}
}

// The live-status poll covers the local reader even with every UniFi
// integration off, flags the rpi source as covered, and (after a scan)
// carries the last tag so an open panel refreshes without a reload.
func TestAdminUA_StatusIncludesReader(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	seedReader(t, env, readerA)

	fetchStatus := func() (out struct {
		OK     bool            `json:"ok"`
		Counts struct{ Online, Offline, Total int } `json:"counts"`
		Items  []struct {
			Kind    string `json:"kind"`
			ID      string `json:"id"`
			Status  string `json:"status"`
			Tag     string `json:"tag"`
			TagSeen string `json:"tagSeen"`
		} `json:"items"`
		Sources map[string]bool `json:"sources"`
	}) {
		t.Helper()
		resp, err := env.client.Get(env.ts.URL + "/a/devices/status")
		if err != nil {
			t.Fatalf("GET status: %v", err)
		}
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	out := fetchStatus()
	if !out.OK {
		t.Fatalf("ok=false with a registered reader")
	}
	if out.Counts.Online != 1 || out.Counts.Total != 1 {
		t.Errorf("counts = %+v, want online=1 total=1", out.Counts)
	}
	if len(out.Items) != 1 || out.Items[0].Kind != "rpi-reader" || out.Items[0].ID != "nfc:i2c-1" || out.Items[0].Status != "online" {
		t.Errorf("items = %+v, want one online rpi-reader nfc:i2c-1", out.Items)
	}
	if out.Items[0].Tag != "" {
		t.Errorf("tag = %q before any scan, want empty", out.Items[0].Tag)
	}
	if !out.Sources["rpi"] || out.Sources["ua"] || out.Sources["protect"] {
		t.Errorf("sources = %v, want rpi only", out.Sources)
	}

	if err := env.readerStore.NoteTag(context.Background(), "nfc:i2c-1", "04:A3:1B:2C"); err != nil {
		t.Fatalf("NoteTag: %v", err)
	}
	out = fetchStatus()
	if len(out.Items) != 1 || out.Items[0].Tag != "04:A3:1B:2C" || out.Items[0].TagSeen == "" {
		t.Errorf("items after scan = %+v, want tag 04:A3:1B:2C with a timestamp", out.Items)
	}
}
