// Tests for the inline-settings extension (the auto_screensaver
// form field) and the /webviewer/history.json endpoint.
// Together they cover the runtime-facing surface the
// modes-container relies on.
package httpserver

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"carvilon.local/server/internal/doorhistory"
	"carvilon.local/server/internal/uaapi"
)

// TestMieterSettingsPost_AutoScreensaverJSON exercises the JSON
// branch (Accept: application/json) that the inline-settings form
// uses. Verifies the response envelope and the persisted value
// in viewermanager.
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

	info, err := env.viewerMgr.GetViewerInfo(t.Context(), testViewerMAC)
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

	info, err := env.viewerMgr.GetViewerInfo(t.Context(), testViewerMAC)
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
	if err := env.viewerMgr.SetAutoScreensaverSeconds(t.Context(), testViewerMAC, 300); err != nil {
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

	info, err := env.viewerMgr.GetViewerInfo(t.Context(), testViewerMAC)
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
		ViewerMAC:     testViewerMAC,
		EventType:   doorhistory.TypeDoorbellStart,
		IntercomMAC: "28:70:4e:31:e2:9c",
		OccurredAt:  occurred,
	}, nil); err != nil {
		t.Fatalf("seed insert 1: %v", err)
	}
	if _, err := env.history.Insert(ctx, doorhistory.Event{
		ViewerMAC:     testViewerMAC,
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

// TestMieterHistoryJSON_DoorNameResolved covers the door-name
// resolution: when UA-API knows the intercom MAC, the response
// must surface the friendly door name (not the bare MAC, which
// was an earlier stop-gap).
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
		ViewerMAC:     testViewerMAC,
		EventType:   doorhistory.TypeDoorbellStart,
		IntercomMAC: "28:70:4e:31:e2:9c",
		OccurredAt:  time.Now(),
	}, nil); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	// One event with no intercom MAC -> generic Hauseingang.
	if _, err := env.history.Insert(t.Context(), doorhistory.Event{
		ViewerMAC:    testViewerMAC,
		EventType:  doorhistory.TypeDoorbellStart,
		OccurredAt: time.Now().Add(-1 * time.Minute),
	}, nil); err != nil {
		t.Fatalf("seed insert empty mac: %v", err)
	}
	// One event with an intercom MAC that UA does NOT know ->
	// last-resort bare MAC.
	if _, err := env.history.Insert(t.Context(), doorhistory.Event{
		ViewerMAC:     testViewerMAC,
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
	//
	// In this single-door stub ALL three rows resolve to the
	// only door's name - including the unknown MAC, which an
	// earlier resolver mapped to the generic label.
	type want struct{ intercom, name string }
	wants := []want{
		{intercom: "28:70:4e:31:e2:9c", name: "Door door-uuid-front"},
		{intercom: "", name: "Door door-uuid-front"},
		{intercom: "11:22:33:44:55:66", name: "Door door-uuid-front"},
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

// TestMieterSettingsPost_AcceptsScreenOff verifies that the
// canonical mieter POST handler accepts the
// third idle_view_mode value (ESP-only concept but pass-through-
// safe for the web viewer).
func TestMieterSettingsPost_AcceptsScreenOff(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)

	form := url.Values{}
	form.Set("idle_view_mode", "screen_off")
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

	info, err := env.viewerMgr.GetViewerInfo(t.Context(), testViewerMAC)
	if err != nil {
		t.Fatalf("GetViewerInfo: %v", err)
	}
	if info.ResolveIdleViewMode() != "screen_off" {
		t.Errorf("persisted idle_view_mode = %q, want screen_off",
			info.ResolveIdleViewMode())
	}
}

// TestMieterSettingsPost_HistoryCaptureToggle covers the
// capture-disable surface. Setting "0" flips the toggle and the
// next /webviewer/history.json call returns
// capture_enabled:false + empty events.
func TestMieterSettingsPost_HistoryCaptureToggle(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)

	form := url.Values{}
	form.Set("idle_view_mode", "screensaver")
	form.Set("history_capture", "0")
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
	if got, _ := out["history_capture"].(bool); got != false {
		t.Errorf("history_capture echo = %v, want false", out["history_capture"])
	}

	info, err := env.viewerMgr.GetViewerInfo(t.Context(), testViewerMAC)
	if err != nil {
		t.Fatalf("GetViewerInfo: %v", err)
	}
	if info.ResolveHistoryCaptureEnabled() {
		t.Errorf("after Set(0) capture still enabled")
	}
}

func TestMieterSettingsPost_ClockLayoutPersists(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)

	form := url.Values{}
	form.Set("clock_layout", "horizontal")
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
	info, _ := env.viewerMgr.GetViewerInfo(t.Context(), testViewerMAC)
	if info.ResolveClockLayout() != "horizontal" {
		t.Errorf("persisted clock_layout = %q, want horizontal",
			info.ResolveClockLayout())
	}
}

func TestMieterSettingsPost_ClockLayoutRejectsBogus(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)
	form := url.Values{}
	form.Set("clock_layout", "diagonal")
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/webviewer/settings",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestMieterSettingsPost_HistoryCaptureRejectsBogus(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)

	form := url.Values{}
	form.Set("idle_view_mode", "screensaver")
	form.Set("history_capture", "maybe")
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/webviewer/settings",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestMieterSettingsPost_BroadcastsConfigChanged verifies that a
// successful POST raises a doorbellhub config.changed event so
// other tabs / browser sessions on the same viewer_mac pick up
// the new state via SSE.
func TestMieterSettingsPost_BroadcastsConfigChanged(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)

	sub, cleanup := env.hub.Subscribe(testViewerMAC)
	defer cleanup()

	form := url.Values{}
	form.Set("idle_view_mode", "livestream")
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

	select {
	case ev := <-sub.Events:
		if ev.Type != "config.changed" {
			t.Errorf("ev.Type = %q, want config.changed", ev.Type)
		}
		if ev.ViewerMAC != testViewerMAC {
			t.Errorf("ev.ViewerMAC = %q, want %q", ev.ViewerMAC, testViewerMAC)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("subscriber did not receive config.changed")
	}
}

// TestMieterHistoryJSON_RequiresSession the JSON endpoint must be
// behind requireViewerAuth; without a cookie it redirects to /login.
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

// ---------- history pagination + filter ----------

func TestMieterHistoryJSON_Pagination(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)

	// Seed 5 events.
	ctx := t.Context()
	base := time.Now()
	for i := 0; i < 5; i++ {
		if _, err := env.history.Insert(ctx, doorhistory.Event{
			ViewerMAC:    testViewerMAC,
			EventType:  doorhistory.TypeDoorbellStart,
			OccurredAt: base.Add(time.Duration(-i) * time.Hour),
		}, nil); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	resp, err := env.client.Get(env.ts.URL + "/webviewer/history.json?limit=2&offset=0")
	if err != nil {
		t.Fatalf("page1 GET: %v", err)
	}
	var p1 mieterHistoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&p1); err != nil {
		t.Fatalf("decode page1: %v", err)
	}
	resp.Body.Close()
	if len(p1.Events) != 2 {
		t.Errorf("page1 events = %d, want 2", len(p1.Events))
	}
	if !p1.HasMore {
		t.Errorf("page1 HasMore = false, want true")
	}
	if p1.NextOffset != 2 {
		t.Errorf("page1 NextOffset = %d, want 2", p1.NextOffset)
	}

	resp2, err := env.client.Get(env.ts.URL + "/webviewer/history.json?limit=2&offset=4")
	if err != nil {
		t.Fatalf("last page GET: %v", err)
	}
	var p3 mieterHistoryResponse
	if err := json.NewDecoder(resp2.Body).Decode(&p3); err != nil {
		t.Fatalf("decode last page: %v", err)
	}
	resp2.Body.Close()
	if len(p3.Events) != 1 {
		t.Errorf("last page events = %d, want 1", len(p3.Events))
	}
	if p3.HasMore {
		t.Errorf("last page HasMore = true, want false")
	}
}

func TestMieterHistoryJSON_DateFilter(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)
	ctx := t.Context()
	// Drei Events: vor 7 Tagen, vor 2 Tagen, jetzt.
	now := time.Now()
	for _, when := range []time.Time{
		now.AddDate(0, 0, -7),
		now.AddDate(0, 0, -2),
		now,
	} {
		if _, err := env.history.Insert(ctx, doorhistory.Event{
			ViewerMAC:    testViewerMAC,
			EventType:  doorhistory.TypeDoorbellStart,
			OccurredAt: when,
		}, nil); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	from := now.AddDate(0, 0, -3).Format("2006-01-02")
	resp, err := env.client.Get(env.ts.URL + "/webviewer/history.json?from=" + from)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var got mieterHistoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Events) != 2 {
		t.Errorf("from-filter events = %d, want 2", len(got.Events))
	}
}

func TestMieterHistoryJSON_InvalidOffsetRejected(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)
	resp, err := env.client.Get(env.ts.URL + "/webviewer/history.json?offset=99999")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestMieterHistoryJSON_InvalidLimitRejected(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)
	resp, err := env.client.Get(env.ts.URL + "/webviewer/history.json?limit=999")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestMieterHistoryJSON_InvalidDateRejected(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)
	resp, err := env.client.Get(env.ts.URL + "/webviewer/history.json?from=garbage")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestMieterHistoryJSON_CaptureDisabledReturnsEmpty(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)

	if _, err := env.history.Insert(t.Context(), doorhistory.Event{
		ViewerMAC:    testViewerMAC,
		EventType:  doorhistory.TypeDoorbellStart,
		OccurredAt: time.Now(),
	}, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := env.viewerMgr.SetHistoryCaptureEnabled(t.Context(), testViewerMAC, false); err != nil {
		t.Fatalf("disable capture: %v", err)
	}

	resp, err := env.client.Get(env.ts.URL + "/webviewer/history.json")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var got mieterHistoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.CaptureEnabled {
		t.Errorf("CaptureEnabled = true, want false")
	}
	if len(got.Events) != 0 {
		t.Errorf("Events len = %d, want 0 (capture disabled)", len(got.Events))
	}
	if got.HasMore {
		t.Errorf("HasMore = true, want false")
	}
}

// ---------- DELETE /webviewer/history* ----------

func TestMieterHistoryDeleteOne_SoftHidesAndDropsOutOfList(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)

	id, err := env.history.Insert(t.Context(), doorhistory.Event{
		ViewerMAC:    testViewerMAC,
		EventType:  doorhistory.TypeDoorbellStart,
		OccurredAt: time.Now(),
	}, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	req, _ := http.NewRequest(http.MethodDelete,
		env.ts.URL+"/webviewer/history/"+strconv.FormatInt(id, 10), nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}

	// Mieter-Liste leer.
	resp2, err := env.client.Get(env.ts.URL + "/webviewer/history.json")
	if err != nil {
		t.Fatalf("GET history: %v", err)
	}
	var got mieterHistoryResponse
	_ = json.NewDecoder(resp2.Body).Decode(&got)
	resp2.Body.Close()
	if len(got.Events) != 0 {
		t.Errorf("event still visible after DELETE (len=%d)", len(got.Events))
	}
	// door_events bleibt fuer Admin-Audit-Trail.
	visible, _ := env.history.CountVisible(t.Context(), testViewerMAC, doorhistory.ListOpts{})
	if visible != 0 {
		t.Errorf("CountVisible = %d, want 0", visible)
	}
}

func TestMieterHistoryDeleteOne_RejectsBogusID(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)

	req, _ := http.NewRequest(http.MethodDelete,
		env.ts.URL+"/webviewer/history/abc", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestMieterHistoryDeleteOne_OtherViewerCannotHide(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)

	// Seed an event for ANOTHER viewer.
	env.seedViewerAs(t, "0c:ea:14:bb:cc:dd", "Other Viewer", "TestPw-1234567X")
	id, err := env.history.Insert(t.Context(), doorhistory.Event{
		ViewerMAC:    "0c:ea:14:bb:cc:dd",
		EventType:  doorhistory.TypeDoorbellStart,
		OccurredAt: time.Now(),
	}, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	req, _ := http.NewRequest(http.MethodDelete,
		env.ts.URL+"/webviewer/history/"+strconv.FormatInt(id, 10), nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (cross-viewer hide)", resp.StatusCode)
	}
}

func TestMieterHistoryDeleteAll_HidesEverythingForMieter(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)
	for i := 0; i < 3; i++ {
		if _, err := env.history.Insert(t.Context(), doorhistory.Event{
			ViewerMAC:    testViewerMAC,
			EventType:  doorhistory.TypeDoorbellStart,
			OccurredAt: time.Now().Add(time.Duration(-i) * time.Hour),
		}, nil); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	req, _ := http.NewRequest(http.MethodDelete, env.ts.URL+"/webviewer/history", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("DELETE all: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}
	var out struct {
		OK          bool `json:"ok"`
		HiddenCount int  `json:"hidden_count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.OK || out.HiddenCount != 3 {
		t.Errorf("response = %+v, want ok=true hidden_count=3", out)
	}
	// Admin-Side bleibt unangetastet.
	res, _ := env.history.AdminListAll(t.Context(), testViewerMAC, doorhistory.ListOpts{})
	if res.TotalCount != 3 {
		t.Errorf("admin TotalCount = %d, want 3 (unaffected)", res.TotalCount)
	}
	if res.HiddenCount != 3 {
		t.Errorf("admin HiddenCount = %d, want 3 (all flagged)", res.HiddenCount)
	}
}

func TestMieterHistoryDeleteOne_DropsUnreadCount(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)

	id, err := env.history.Insert(t.Context(), doorhistory.Event{
		ViewerMAC:    testViewerMAC,
		EventType:  doorhistory.TypeDoorbellStart,
		OccurredAt: time.Now(),
	}, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Vorab: 1 unread.
	before, _ := env.history.UnreadCount(t.Context(), testViewerMAC)
	if before != 1 {
		t.Fatalf("seeded UnreadCount = %d, want 1", before)
	}

	req, _ := http.NewRequest(http.MethodDelete,
		env.ts.URL+"/webviewer/history/"+strconv.FormatInt(id, 10), nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	resp.Body.Close()

	after, _ := env.history.UnreadCount(t.Context(), testViewerMAC)
	if after != 0 {
		t.Errorf("UnreadCount after hide = %d, want 0", after)
	}
}

func TestMieterHistoryJSON_HiddenEventsAreFilteredOut(t *testing.T) {
	env := newTestServer(t)
	loginMieterForTest(t, env)

	id, err := env.history.Insert(t.Context(), doorhistory.Event{
		ViewerMAC:    testViewerMAC,
		EventType:  doorhistory.TypeDoorbellStart,
		OccurredAt: time.Now(),
	}, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := env.history.HideEvent(t.Context(), testViewerMAC, id); err != nil {
		t.Fatalf("HideEvent: %v", err)
	}
	resp, err := env.client.Get(env.ts.URL + "/webviewer/history.json")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var got mieterHistoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Events) != 0 {
		t.Errorf("hidden event still in response (len=%d)", len(got.Events))
	}
}
