package httpserver

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"carvilon.local/server/internal/platformconfig"
)

func postAccent(t *testing.T, env *testEnv, hex string) *http.Response {
	t.Helper()
	form := url.Values{}
	form.Set("accent", hex)
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/a/settings/accent", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /a/settings/accent: %v", err)
	}
	return resp
}

// TestAdminAccent_PersistsAndInjects proves the accent picker stores ONE value
// in platform_config (no migration) and that it recolors both a new-design
// page (--accent in the layout) and a legacy page (--color-accent in the nav).
func TestAdminAccent_PersistsAndInjects(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	resp := postAccent(t, env, "#3D7BFF") // mixed case -> normalized lower
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("accent post status = %d", resp.StatusCode)
	}
	got, _ := env.platformCfg.Get(context.Background(), platformconfig.KeyAdminAccentColor)
	if got != "#3d7bff" {
		t.Errorf("stored accent = %q, want #3d7bff (normalized)", got)
	}

	// New-design page (viewer-detail) injects --accent via the layout.
	detail, err := env.client.Get(env.ts.URL + "/a/viewers/" + testViewerMAC)
	if err != nil {
		t.Fatalf("GET detail: %v", err)
	}
	defer detail.Body.Close()
	body := readBody(t, detail)
	if !contains(body, "--accent: #3d7bff") {
		t.Errorf("new-design page missing injected --accent override")
	}
	if !contains(body, "/static/admin-tokens.css") || !contains(body, `class="topbar"`) {
		t.Errorf("new-design page missing the new shell (tokens link / topbar)")
	}

	// Legacy page (web-viewers) injects --color-accent via the nav.
	legacy, err := env.client.Get(env.ts.URL + "/a/web-viewers")
	if err != nil {
		t.Fatalf("GET web-viewers: %v", err)
	}
	defer legacy.Body.Close()
	lbody := readBody(t, legacy)
	if !contains(lbody, "--color-accent: #3d7bff") {
		t.Errorf("legacy page missing injected --color-accent override")
	}
}

// TestAdminAccent_DefaultOrange proves an unset accent renders the orange
// default on the new shell.
func TestAdminAccent_DefaultOrange(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	resp, err := env.client.Get(env.ts.URL + "/a/viewers/" + testViewerMAC)
	if err != nil {
		t.Fatalf("GET detail: %v", err)
	}
	defer resp.Body.Close()
	if !contains(readBody(t, resp), "--accent: "+DefaultAccentColor) {
		t.Errorf("default accent %s not injected", DefaultAccentColor)
	}
}

// TestAdminAccent_RejectsBad proves a non-hex value is refused (error flash)
// and the stored value is unchanged.
func TestAdminAccent_RejectsBad(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	resp := postAccent(t, env, "not-a-color")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (re-render with error)", resp.StatusCode)
	}
	if !contains(readBody(t, resp), "Ungueltige Farbe") {
		t.Errorf("expected validation error flash")
	}
	if got, _ := env.platformCfg.Get(context.Background(), platformconfig.KeyAdminAccentColor); got != "" {
		t.Errorf("bad accent must not persist, got %q", got)
	}
}

// TestAdminAccent_PickerOnSettings proves the picker UI renders on /a/settings.
func TestAdminAccent_PickerOnSettings(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	resp, err := env.client.Get(env.ts.URL + "/a/settings")
	if err != nil {
		t.Fatalf("GET settings: %v", err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !contains(body, `action="/a/settings/accent"`) {
		t.Errorf("accent picker form missing on settings page")
	}
	if !contains(body, `value="#ff7a1a"`) {
		t.Errorf("orange preset swatch missing")
	}
	if !contains(body, `type="color"`) {
		t.Errorf("custom color input missing")
	}
}
