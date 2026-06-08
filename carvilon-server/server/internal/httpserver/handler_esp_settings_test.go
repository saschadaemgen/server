// Tests for POST /esp/settings.
//
// Bearer auth, partial update, strict allow-lists, config.changed
// SSE broadcast. Mirrors the test patterns from esp_runtime_test.go
// (bearer adoption flow) and handler_mieter_unread_test.go (SSE
// observation against the test env's hub).
package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func postESPSettings(t *testing.T, env *testEnv, token string, payload any) *http.Response {
	t.Helper()
	buf, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/esp/settings", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /esp/settings: %v", err)
	}
	return resp
}

func TestESPSettings_RequiresBearerAuth(t *testing.T) {
	env := newTestServer(t)
	resp := postESPSettings(t, env, "", map[string]any{"language": "de"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestESPSettings_FullUpdate(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung Settings A")

	resp := postESPSettings(t, env, tok, map[string]any{
		"idle_view_mode":           "screensaver",
		"auto_screensaver_seconds": 60,
		"screen_off_after_sec":     300,
		"brightness_idle":          70,
		"language":                 "de",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}
	var out struct {
		OK      bool           `json:"ok"`
		Applied map[string]any `json:"applied"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.OK {
		t.Errorf("ok = false")
	}
	for _, k := range []string{
		"idle_view_mode", "auto_screensaver_seconds",
		"screen_off_after_sec", "brightness_idle", "language",
	} {
		if _, ok := out.Applied[k]; !ok {
			t.Errorf("applied missing %q", k)
		}
	}

	info, err := env.viewerMgr.GetViewerInfo(context.Background(), espTestMAC)
	if err != nil {
		t.Fatalf("GetViewerInfo: %v", err)
	}
	if info.ResolveIdleViewMode() != "screensaver" {
		t.Errorf("IdleViewMode = %q", info.ResolveIdleViewMode())
	}
	if info.ResolveAutoScreensaverSeconds() != 60 {
		t.Errorf("AutoScreensaverSeconds = %d", info.ResolveAutoScreensaverSeconds())
	}
	if info.ResolveScreenOffAfterSec() != 300 {
		t.Errorf("ScreenOffAfterSec = %d", info.ResolveScreenOffAfterSec())
	}
	if info.ResolveBrightnessIdle() != 70 {
		t.Errorf("BrightnessIdle = %d", info.ResolveBrightnessIdle())
	}
	if info.ResolveLanguage() != "de" {
		t.Errorf("Language = %q", info.ResolveLanguage())
	}
}

func TestESPSettings_PartialUpdate(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung Settings B")

	// First arm an initial brightness so we can verify the
	// partial update leaves it untouched.
	if err := env.viewerMgr.SetBrightnessIdle(context.Background(), espTestMAC, 42); err != nil {
		t.Fatalf("seed brightness: %v", err)
	}

	resp := postESPSettings(t, env, tok, map[string]any{"language": "en"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}

	info, _ := env.viewerMgr.GetViewerInfo(context.Background(), espTestMAC)
	if info.ResolveLanguage() != "en" {
		t.Errorf("Language = %q, want en", info.ResolveLanguage())
	}
	if info.ResolveBrightnessIdle() != 42 {
		t.Errorf("Brightness untouched? got %d, want 42",
			info.ResolveBrightnessIdle())
	}
}

func TestESPSettings_InvalidBrightnessRejected(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung Settings C")

	resp := postESPSettings(t, env, tok, map[string]any{"brightness_idle": 150})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestESPSettings_InvalidIdleViewModeRejected(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung Settings D")

	resp := postESPSettings(t, env, tok, map[string]any{"idle_view_mode": "foo"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestESPSettings_AcceptsScreenOff(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung Settings E")

	resp := postESPSettings(t, env, tok, map[string]any{
		"idle_view_mode": "screen_off",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}
	info, _ := env.viewerMgr.GetViewerInfo(context.Background(), espTestMAC)
	if info.ResolveIdleViewMode() != "screen_off" {
		t.Errorf("IdleViewMode = %q, want screen_off", info.ResolveIdleViewMode())
	}
}

func TestESPSettings_TriggersConfigChangedOnESPBus(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung Settings F")

	bus := env.srv.EventBus()
	sub := bus.Subscribe(espTestMAC)
	defer bus.Unsubscribe(espTestMAC, sub)

	resp := postESPSettings(t, env, tok, map[string]any{"language": "de"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	select {
	case ev := <-sub:
		if ev.Type != "config.changed" {
			t.Errorf("ev.Type = %q, want config.changed", ev.Type)
		}
		if ev.JSON != "{}" {
			t.Errorf("ev.JSON = %q, want %q", ev.JSON, "{}")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("config.changed not pushed to eventbus")
	}
}

func TestESPSettings_AcceptsClockLayout(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung Clock A")

	resp := postESPSettings(t, env, tok, map[string]any{"clock_layout": "horizontal"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}
	info, _ := env.viewerMgr.GetViewerInfo(context.Background(), espTestMAC)
	if info.ResolveClockLayout() != "horizontal" {
		t.Errorf("persisted clock_layout = %q, want horizontal",
			info.ResolveClockLayout())
	}
}

func TestESPSettings_RejectsBogusClockLayout(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung Clock B")

	resp := postESPSettings(t, env, tok, map[string]any{"clock_layout": "diagonal"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestESPSettings_EmptyBodyNoBroadcast(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung Settings G")

	bus := env.srv.EventBus()
	sub := bus.Subscribe(espTestMAC)
	defer bus.Unsubscribe(espTestMAC, sub)

	// POST mit leerem JSON-Body soll keinen Broadcast triggern.
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/esp/settings",
		strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	select {
	case ev := <-sub:
		t.Errorf("got unexpected broadcast %+v", ev)
	case <-time.After(120 * time.Millisecond):
		// expected
	}
}
