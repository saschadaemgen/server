// Saison 14-03 tests for the inline-settings extension (the
// auto_screensaver form field) and the new /webviewer/history.json
// endpoint. Together they cover the runtime-facing surface the
// modes-container relies on.
package httpserver

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"unifix.local/server/internal/doorhistory"
	"unifix.local/server/internal/uaapi"
)

// TestMieterSettingsPost_AutoScreensaverJSON exercises the JSON
// branch (Accept: application/json) that the inline-settings form
// uses. Verifies the response envelope and the persisted value
// in mockmanager.
func TestMieterSettingsPost_AutoScreensaverJSON(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)

	form := url.Values{}
	form.Set("idle_view_mode", "screensaver")
	form.Set("auto_screensaver", "60")
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/webviewer/settings",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /webviewer/settings: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}

	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["ok"] != true {
		t.Errorf("ok = %v, want true", out["ok"])
	}
	if out["idle_view_mode"] != "screensaver" {
		t.Errorf("idle_view_mode = %v, want screensaver", out["idle_view_mode"])
	}
	if got, _ := out["auto_screensaver_seconds"].(float64); int(got) != 60 {
		t.Errorf("auto_screensaver_seconds = %v, want 60", out["auto_screensaver_seconds"])
	}

	info, err := env.mockMgr.GetViewerInfo(t.Context(), testViewerMAC)
	if err != nil {
		t.Fatalf("GetViewerInfo: %v", err)
	}
	if got := info.ResolveAutoScreensaverSeconds(); got != 60 {
		t.Errorf("persisted auto_screensaver_seconds = %d, want 60", got)
	}
}

// TestMieterSettingsPost_AutoScreensaverInvalid checks that the
// handler rejects values outside the allow-list. 999 is not in
// {0, 30, 60, 300, 600}; the server must respond 400 and leave
// the persisted value untouched.
func TestMieterSettingsPost_AutoScreensaverInvalid(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)

	form := url.Values{}
	form.Set("idle_view_mode", "screensaver")
	form.Set("auto_screensaver", "999")
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/webviewer/settings",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /webviewer/settings: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400, body=%s", resp.StatusCode, readBody(t, resp))
	}

	info, err := env.mockMgr.GetViewerInfo(t.Context(), testViewerMAC)
	if err != nil {
		t.Fatalf("GetViewerInfo: %v", err)
	}
	if got := info.ResolveAutoScreensaverSeconds(); got != 0 {
		t.Errorf("persisted auto_screensaver_seconds = %d, want 0 (untouched)", got)
	}
}

// TestMieterSettingsPost_AutoScreensaverZeroDisables stores 0 and
// expects ResolveAutoScreensaverSeconds to return 0 (NULL in DB).
func TestMieterSettingsPost_AutoScreensaverZeroDisables(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)

	// First arm the timer.
	if err := env.mockMgr.SetAutoScreensaverSeconds(t.Context(), testViewerMAC, 300); err != nil {
		t.Fatalf("seed: SetAutoScreensaverSeconds: %v", err)
	}

	// Then disable via the public POST surface.
	form := url.Values{}
	form.Set("idle_view_mode", "screensaver")
	form.Set("auto_screensaver", "0")
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

	info, err := env.mockMgr.GetViewerInfo(t.Context(), testViewerMAC)
	if err != nil {
		t.Fatalf("GetViewerInfo: %v", err)
	}
	if got := info.ResolveAutoScreensaverSeconds(); got != 0 {
		t.Errorf("auto_screensaver_seconds after disable = %d, want 0", got)
	}
}

// TestMieterHistoryJSON_EmptyAndPopulated verifies the JSON
// envelope shape and the mark-read side effect: after the call,
// previously-unread rows are no longer flagged unread.
func TestMieterHistoryJSON_EmptyAndPopulated(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)

	// Empty case first.
	resp, err := env.client.Get(env.ts.URL + "/webviewer/history.json")
	if err != nil {
		t.Fatalf("GET history.json: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var emptyOut mieterHistoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&emptyOut); err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	resp.Body.Close()
	if emptyOut.Events == nil {
		t.Errorf("empty Events should be [] not null")
	}
	if len(emptyOut.Events) != 0 {
		t.Errorf("empty Events len = %d, want 0", len(emptyOut.Events))
	}

	// Seed two events: one cancel (unread), one start (unread).
	ctx := t.Context()
	occurred := time.Now()
	if _, err := env.history.Insert(ctx, doorhistory.Event{
		MockMAC:     testViewerMAC,
		EventType:   doorhistory.TypeDoorbellStart,
		IntercomMAC: "28:70:4e:31:e2:9c",
		OccurredAt:  occurred,
	}, nil); err != nil {
		t.Fatalf("seed insert 1: %v", err)
	}
	if _, err := env.history.Insert(ctx, doorhistory.Event{
		MockMAC:     testViewerMAC,
		EventType:   doorhistory.TypeDoorbellCancel,
		IntercomMAC: "28:70:4e:31:e2:9c",
		OccurredAt:  occurred.Add(-1 * time.Minute),
	}, nil); err != nil {
		t.Fatalf("seed insert 2: %v", err)
	}

	// First fetch should see both as unread.
	resp2, err := env.client.Get(env.ts.URL + "/webviewer/history.json")
	if err != nil {
		t.Fatalf("GET history.json (populated): %v", err)
	}
	var out mieterHistoryResponse
	if err := json.NewDecoder(resp2.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp2.Body.Close()
	if len(out.Events) != 2 {
		t.Fatalf("events len = %d, want 2", len(out.Events))
	}
	unread := 0
	for _, ev := range out.Events {
		if ev.Unread {
			unread++
		}
		if ev.IntercomMAC != "28:70:4e:31:e2:9c" {
			t.Errorf("intercom_mac = %q, want 28:70:4e:31:e2:9c", ev.IntercomMAC)
		}
		if ev.EventType == "" {
			t.Errorf("event_type empty")
		}
		if ev.When == "" {
			t.Errorf("when empty")
		}
	}
	if unread != 2 {
		t.Errorf("unread count first fetch = %d, want 2", unread)
	}

	// The mark-read is asynchronous; wait until the unread count
	// drops to 0 or fail after a generous timeout.
	deadline := time.Now().Add(2 * time.Second)
	for {
		n, err := env.history.UnreadCount(ctx, testViewerMAC)
		if err != nil {
			t.Fatalf("UnreadCount: %v", err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("UnreadCount still %d after timeout", n)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Second fetch sees zero unread.
	resp3, err := env.client.Get(env.ts.URL + "/webviewer/history.json")
	if err != nil {
		t.Fatalf("GET history.json (after read): %v", err)
	}
	var out3 mieterHistoryResponse
	if err := json.NewDecoder(resp3.Body).Decode(&out3); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp3.Body.Close()
	if len(out3.Events) != 2 {
		t.Fatalf("events len (after read) = %d, want 2", len(out3.Events))
	}
	for _, ev := range out3.Events {
		if ev.Unread {
			t.Errorf("event %d still unread after read-mark", ev.ID)
		}
	}
}

// TestMieterHistoryJSON_DoorNameResolved covers the saison-14-03-FIX02
// Sub-1a fix: when UA-API knows the intercom MAC, the response
// must surface the friendly door name (not the bare MAC, which
// was the pre-FIX02 stop-gap).
func TestMieterHistoryJSON_DoorNameResolved(t *testing.T) {
	uaStub := newUADoorsStub(t, uaDoorStubConfig{
		doors: map[string]string{
			"door-uuid-front": "28704e31e29c",
		},
	}, nil)
	defer uaStub.Close()

	env := newTestServer(t)
	loginMieterForTest(t, env)
	env.srv.SetUAClient(uaapi.New(uaapi.Options{BaseURL: uaStub.URL, Token: "t"}))

	// One event whose intercom MAC matches the stub.
	if _, err := env.history.Insert(t.Context(), doorhistory.Event{
		MockMAC:     testViewerMAC,
		EventType:   doorhistory.TypeDoorbellStart,
		IntercomMAC: "28:70:4e:31:e2:9c",
		OccurredAt:  time.Now(),
	}, nil); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	// One event with no intercom MAC -> generic Hauseingang.
	if _, err := env.history.Insert(t.Context(), doorhistory.Event{
		MockMAC:    testViewerMAC,
		EventType:  doorhistory.TypeDoorbellStart,
		OccurredAt: time.Now().Add(-1 * time.Minute),
	}, nil); err != nil {
		t.Fatalf("seed insert empty mac: %v", err)
	}
	// One event with an intercom MAC that UA does NOT know ->
	// last-resort bare MAC.
	if _, err := env.history.Insert(t.Context(), doorhistory.Event{
		MockMAC:     testViewerMAC,
		EventType:   doorhistory.TypeDoorbellStart,
		IntercomMAC: "11:22:33:44:55:66",
		OccurredAt:  time.Now().Add(-2 * time.Minute),
	}, nil); err != nil {
		t.Fatalf("seed insert unknown mac: %v", err)
	}

	resp, err := env.client.Get(env.ts.URL + "/webviewer/history.json")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	var out mieterHistoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if len(out.Events) != 3 {
		t.Fatalf("events len = %d, want 3", len(out.Events))
	}
	// Events are returned newest-first; order: known MAC, empty
	// MAC, unknown MAC.
	type want struct{ intercom, name string }
	wants := []want{
		{intercom: "28:70:4e:31:e2:9c", name: "Door door-uuid-front"},
		{intercom: "", name: genericDoorName},
		{intercom: "11:22:33:44:55:66", name: "11:22:33:44:55:66"},
	}
	for i, ev := range out.Events {
		if ev.IntercomMAC != wants[i].intercom {
			t.Errorf("event %d intercom_mac = %q, want %q", i, ev.IntercomMAC, wants[i].intercom)
		}
		if ev.DoorName != wants[i].name {
			t.Errorf("event %d door_name = %q, want %q", i, ev.DoorName, wants[i].name)
		}
	}
}

// TestMieterHistoryJSON_RequiresSession the JSON endpoint must be
// behind requireSession; without a cookie it redirects to /login.
func TestMieterHistoryJSON_RequiresSession(t *testing.T) {
	env := newTestServer(t)
	resp, err := env.client.Get(env.ts.URL + "/webviewer/history.json")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303 (redirect to /login)", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Errorf("Location = %q, want /login", loc)
	}
}
