// Saison 14-03-FIX03 Sub-2: coverage for the unread-doorbell
// surface (GET endpoint, SSE broadcast on new event + on
// mark-read) and for the canonical auto_screensaver_seconds
// settings-form field (Sub-1a Naming-Fix).
package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"carvilon.local/server/internal/doorbellhub"
	"carvilon.local/server/internal/doorhistory"
)

// TestMieterUnreadCount_Endpoint exercises the read endpoint:
// fresh viewer with no events -> 0; after seeding two events
// -> 2; after one of them is marked read -> 1.
func TestMieterUnreadCount_Endpoint(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)

	// Empty case.
	resp, err := env.client.Get(env.ts.URL + "/webviewer/unread-count")
	if err != nil {
		t.Fatalf("GET unread-count empty: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var emptyOut mieterUnreadResponse
	if err := json.NewDecoder(resp.Body).Decode(&emptyOut); err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	resp.Body.Close()
	if emptyOut.Count != 0 {
		t.Errorf("empty count = %d, want 0", emptyOut.Count)
	}

	// Seed two unread doorbell_start events.
	ctx := t.Context()
	occurred := time.Now()
	id1, err := env.history.Insert(ctx, doorhistory.Event{
		MockMAC:    testViewerMAC,
		EventType:  doorhistory.TypeDoorbellStart,
		OccurredAt: occurred,
	}, nil)
	if err != nil {
		t.Fatalf("seed insert 1: %v", err)
	}
	if _, err := env.history.Insert(ctx, doorhistory.Event{
		MockMAC:    testViewerMAC,
		EventType:  doorhistory.TypeDoorbellStart,
		OccurredAt: occurred.Add(time.Minute),
	}, nil); err != nil {
		t.Fatalf("seed insert 2: %v", err)
	}

	resp, err = env.client.Get(env.ts.URL + "/webviewer/unread-count")
	if err != nil {
		t.Fatalf("GET unread-count populated: %v", err)
	}
	var out mieterUnreadResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode populated: %v", err)
	}
	resp.Body.Close()
	if out.Count != 2 {
		t.Errorf("populated count = %d, want 2", out.Count)
	}

	// Mark one as read; count drops to 1.
	if err := env.history.MarkRead(ctx, testViewerMAC, []int64{id1}); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	resp, err = env.client.Get(env.ts.URL + "/webviewer/unread-count")
	if err != nil {
		t.Fatalf("GET unread-count after read: %v", err)
	}
	var afterRead mieterUnreadResponse
	if err := json.NewDecoder(resp.Body).Decode(&afterRead); err != nil {
		t.Fatalf("decode after-read: %v", err)
	}
	resp.Body.Close()
	if afterRead.Count != 1 {
		t.Errorf("after-read count = %d, want 1", afterRead.Count)
	}
}

// TestMieterUnreadCount_RequiresSession the endpoint must be
// behind requireSession; without a cookie it redirects to /login.
func TestMieterUnreadCount_RequiresSession(t *testing.T) {
	env := newTestServer(t)
	resp, err := env.client.Get(env.ts.URL + "/webviewer/unread-count")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303 (redirect to /login)", resp.StatusCode)
	}
}

// TestEvents_StreamsUnreadCount checks that BroadcastUnreadCount
// reaches the SSE subscriber with the expected payload shape.
func TestEvents_StreamsUnreadCount(t *testing.T) {
	env := newTestServer(t)
	br, _, cancel := loginAndOpenEvents(t, env, testViewerMAC)
	defer cancel()

	// Seed one unread row directly so UnreadCount returns 1.
	if _, err := env.history.Insert(t.Context(), doorhistory.Event{
		MockMAC:    testViewerMAC,
		EventType:  doorhistory.TypeDoorbellStart,
		OccurredAt: time.Now(),
	}, nil); err != nil {
		t.Fatalf("seed insert: %v", err)
	}

	// Trigger a broadcast directly (no doorbell flow needed for
	// this test; just verifies wire format).
	env.hub.BroadcastUnreadCount(context.Background(), testViewerMAC)

	name, data := nextSSEEvent(t, br, 2*time.Second)
	if name != doorbellhub.TypeUnreadCount {
		t.Errorf("event name = %q, want %q", name, doorbellhub.TypeUnreadCount)
	}
	var payload struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("decode: %v (raw: %s)", err, data)
	}
	if payload.Count != 1 {
		t.Errorf("count = %d, want 1", payload.Count)
	}
}

// TestMieterSettingsPost_CanonicalAutoScreensaverSecondsField
// covers the FIX03 Sub-1a frontend-rename. Posting with the
// canonical "auto_screensaver_seconds" key works (was previously
// "auto_screensaver"); the legacy alias is still accepted by
// the existing TestMieterSettingsPost_AutoScreensaverJSON test.
func TestMieterSettingsPost_CanonicalAutoScreensaverSecondsField(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)

	form := url.Values{}
	form.Set("idle_view_mode", "livestream")
	form.Set("auto_screensaver_seconds", "60")
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/webviewer/settings",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["idle_view_mode"] != "livestream" {
		t.Errorf("idle_view_mode = %v, want livestream", out["idle_view_mode"])
	}
	if got, _ := out["auto_screensaver_seconds"].(float64); int(got) != 60 {
		t.Errorf("auto_screensaver_seconds = %v, want 60", out["auto_screensaver_seconds"])
	}

	// Persisted via canonical key.
	info, err := env.mockMgr.GetViewerInfo(t.Context(), testViewerMAC)
	if err != nil {
		t.Fatalf("GetViewerInfo: %v", err)
	}
	if got := info.ResolveAutoScreensaverSeconds(); got != 60 {
		t.Errorf("persisted seconds = %d, want 60", got)
	}
}

// TestMieterSettingsPost_BothFieldsCanonicalWins verifies the
// dual-name resolver: when both legacy and canonical keys are
// present, canonical takes precedence.
func TestMieterSettingsPost_BothFieldsCanonicalWins(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)

	form := url.Values{}
	form.Set("idle_view_mode", "screensaver")
	form.Set("auto_screensaver_seconds", "300")
	form.Set("auto_screensaver", "30") // legacy, must lose
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/webviewer/settings",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, _ := out["auto_screensaver_seconds"].(float64); int(got) != 300 {
		t.Errorf("canonical-wins: got %v, want 300", out["auto_screensaver_seconds"])
	}
}

