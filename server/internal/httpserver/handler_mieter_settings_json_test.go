package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// TestMieterSettingsJSON_ReturnsViewerSettings: GET /webviewer/settings.json
// returns the Resolve*() values of the cookie/Bearer-authenticated viewer,
// carries exactly the app fields, and omits the ESP-hardware fields (S19-37).
// The request sends NO MAC - the viewer is identified by the auth context.
func TestMieterSettingsJSON_ReturnsViewerSettings(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env) // seeds + signs in testViewerMAC (no MAC sent by the client)

	if err := env.viewerMgr.SetClockLayout(context.Background(), testViewerMAC, "horizontal"); err != nil {
		t.Fatalf("SetClockLayout: %v", err)
	}
	if err := env.viewerMgr.SetIdleViewMode(context.Background(), testViewerMAC, "livestream"); err != nil {
		t.Fatalf("SetIdleViewMode: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/webviewer/settings.json", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("GET /webviewer/settings.json: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["idle_view_mode"] != "livestream" {
		t.Errorf("idle_view_mode = %v, want livestream", body["idle_view_mode"])
	}
	if body["clock_layout"] != "horizontal" {
		t.Errorf("clock_layout = %v, want horizontal", body["clock_layout"])
	}
	// path_mode defaults to "auto" (Saison 19-39).
	if body["path_mode"] != "auto" {
		t.Errorf("path_mode = %v, want auto (default)", body["path_mode"])
	}
	// resolution_mode defaults to "medium" (Saison 19-42).
	if body["resolution_mode"] != "medium" {
		t.Errorf("resolution_mode = %v, want medium (default)", body["resolution_mode"])
	}
	// Exactly the app fields must be present.
	for _, k := range []string{
		"idle_view_mode", "auto_screensaver_seconds", "clock_layout",
		"language", "history_capture_enabled", "unit_name", "path_mode", "resolution_mode",
	} {
		if _, ok := body[k]; !ok {
			t.Errorf("missing app field %q", k)
		}
	}
	// ESP-hardware / ESP-only fields must NOT leak into the app JSON.
	for _, k := range []string{"screen_off_after_sec", "brightness_idle", "stream", "weather", "cameras"} {
		if _, present := body[k]; present {
			t.Errorf("ESP/hardware field %q must NOT be in the app settings JSON", k)
		}
	}
}

// TestMieterSettingsJSON_RequiresAuth: no session/Bearer -> not 200.
func TestMieterSettingsJSON_RequiresAuth(t *testing.T) {
	env := newTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/webviewer/settings.json", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("status = 200 without auth, want 401/redirect")
	}
}
