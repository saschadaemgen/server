// Tests for the four admin-inline-edit endpoints. Covers
// happy-path, validation rejects, type-scoped constraints (ESP
// fields on web -> 400), config.changed broadcast and one-time
// token reveal.
package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"
)

func postAdminViewerJSON(t *testing.T, env *testEnv, path string, body any) *http.Response {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

// ---------- Stammdaten ----------

func TestAdminViewerStammdaten_RenameAndBroadcast(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	sub, cleanup := env.hub.Subscribe(testViewerMAC)
	defer cleanup()

	resp := postAdminViewerJSON(t, env, "/a/viewers/"+testViewerMAC+"/stammdaten", map[string]any{
		"name": "Wohnung Umbenannt",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}

	info, _ := env.viewerMgr.GetViewerInfo(context.Background(), testViewerMAC)
	if info.Name != "Wohnung Umbenannt" {
		t.Errorf("name = %q, want Wohnung Umbenannt", info.Name)
	}
	// config.changed broadcast
	select {
	case ev := <-sub.Events:
		if ev.Type != "config.changed" {
			t.Errorf("ev.Type = %q, want config.changed", ev.Type)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("rename did not broadcast config.changed")
	}
}

func TestAdminViewerStammdaten_PartialPairedIntercom(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	resp := postAdminViewerJSON(t, env, "/a/viewers/"+testViewerMAC+"/stammdaten", map[string]any{
		"paired_intercom_mac": "28:70:4e:31:e2:9c",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	info, _ := env.viewerMgr.GetViewerInfo(context.Background(), testViewerMAC)
	if info.PairedIntercomMAC != "28:70:4e:31:e2:9c" {
		t.Errorf("paired = %q", info.PairedIntercomMAC)
	}
	// Name unveraendert (partial update).
	if info.Name != testViewerName {
		t.Errorf("name leaked changed: %q", info.Name)
	}
}

func TestAdminViewerStammdaten_RejectsEmptyName(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	resp := postAdminViewerJSON(t, env, "/a/viewers/"+testViewerMAC+"/stammdaten", map[string]any{
		"name": "   ",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAdminViewerStammdaten_RejectsBogusIntercomMAC(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)
	resp := postAdminViewerJSON(t, env, "/a/viewers/"+testViewerMAC+"/stammdaten", map[string]any{
		"paired_intercom_mac": "garbage",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAdminViewerStammdaten_UnknownMAC(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	resp := postAdminViewerJSON(t, env, "/a/viewers/0c:ea:14:00:00:00/stammdaten", map[string]any{
		"name": "X",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// ---------- Settings ----------

func TestAdminViewerSettings_FullWebUpdate(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	resp := postAdminViewerJSON(t, env, "/a/viewers/"+testViewerMAC+"/settings", map[string]any{
		"idle_view_mode":           "livestream",
		"auto_screensaver_seconds": 300,
		"history_capture":          false,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}
	info, _ := env.viewerMgr.GetViewerInfo(context.Background(), testViewerMAC)
	if info.ResolveIdleViewMode() != "livestream" {
		t.Errorf("idle_view_mode = %q", info.ResolveIdleViewMode())
	}
	if info.ResolveAutoScreensaverSeconds() != 300 {
		t.Errorf("auto = %d", info.ResolveAutoScreensaverSeconds())
	}
	if info.ResolveHistoryCaptureEnabled() {
		t.Errorf("history capture still enabled")
	}
}

func TestAdminViewerSettings_AcceptsClockLayout(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	resp := postAdminViewerJSON(t, env, "/a/viewers/"+testViewerMAC+"/settings", map[string]any{
		"clock_layout": "horizontal",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}
	info, _ := env.viewerMgr.GetViewerInfo(context.Background(), testViewerMAC)
	if info.ResolveClockLayout() != "horizontal" {
		t.Errorf("clock_layout = %q, want horizontal", info.ResolveClockLayout())
	}
}

func TestAdminViewerSettings_RejectsBogusClockLayout(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	resp := postAdminViewerJSON(t, env, "/a/viewers/"+testViewerMAC+"/settings", map[string]any{
		"clock_layout": "diagonal",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAdminViewerSettings_ESPOnlyFieldsBlockedOnWeb(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	cases := []map[string]any{
		{"screen_off_after_sec": 60},
		{"brightness_idle": 70},
		{"language": "en"},
	}
	for _, body := range cases {
		resp := postAdminViewerJSON(t, env, "/a/viewers/"+testViewerMAC+"/settings", body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %v -> status %d, want 400 (ESP-only on web viewer)", body, resp.StatusCode)
		}
	}
}

func TestAdminViewerSettings_ESPViewerAcceptsESPFields(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	adoptESPForTest(t, env, espTestMAC, "Wohnung ESP X")

	resp := postAdminViewerJSON(t, env, "/a/viewers/"+espTestMAC+"/settings", map[string]any{
		"screen_off_after_sec": 600,
		"brightness_idle":      42,
		"language":             "en",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}
	info, _ := env.viewerMgr.GetViewerInfo(context.Background(), espTestMAC)
	if info.ResolveScreenOffAfterSec() != 600 {
		t.Errorf("screen_off = %d", info.ResolveScreenOffAfterSec())
	}
	if info.ResolveBrightnessIdle() != 42 {
		t.Errorf("brightness = %d", info.ResolveBrightnessIdle())
	}
	if info.ResolveLanguage() != "en" {
		t.Errorf("language = %q", info.ResolveLanguage())
	}
}

func TestAdminViewerSettings_TriggersConfigChanged(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	sub, cleanup := env.hub.Subscribe(testViewerMAC)
	defer cleanup()

	resp := postAdminViewerJSON(t, env, "/a/viewers/"+testViewerMAC+"/settings", map[string]any{
		"idle_view_mode": "screensaver",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	select {
	case ev := <-sub.Events:
		if ev.Type != "config.changed" {
			t.Errorf("ev.Type = %q", ev.Type)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("config.changed not broadcast")
	}
}

func TestAdminViewerSettings_EmptyBodyNoBroadcast(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	sub, cleanup := env.hub.Subscribe(testViewerMAC)
	defer cleanup()

	resp := postAdminViewerJSON(t, env, "/a/viewers/"+testViewerMAC+"/settings", map[string]any{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	select {
	case ev := <-sub.Events:
		t.Errorf("unexpected broadcast on empty settings POST: %+v", ev)
	case <-time.After(120 * time.Millisecond):
		// expected
	}
}

// ---------- Password ----------

func TestAdminViewerPassword_SetAndInvalidatesSessions(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)
	// Mieter loggt sich erst ein damit es eine Session zum
	// invalidieren gibt.
	loginResp := env.loginViewer(t, testViewerLogin, testViewerPassword)
	loginResp.Body.Close()

	resp := postAdminViewerJSON(t, env, "/a/viewers/"+testViewerMAC+"/password", map[string]any{
		"new_password": "neuesPasswort123",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}

	// Altes Passwort funktioniert nicht mehr.
	old := env.loginViewer(t, testViewerLogin, testViewerPassword)
	defer old.Body.Close()
	if old.StatusCode == http.StatusSeeOther {
		t.Errorf("old password still works after admin reset")
	}
	// Neues Passwort funktioniert. Frischer cookiejar damit das
	// Re-Login einen sauberen Session-Slot bekommt.
	origJar := env.client.Jar
	freshJar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	env.client.Jar = freshJar
	defer func() { env.client.Jar = origJar }()
	good := env.loginViewer(t, testViewerLogin, "neuesPasswort123")
	defer good.Body.Close()
	if good.StatusCode != http.StatusSeeOther {
		t.Errorf("new password did not work, status = %d", good.StatusCode)
	}
}

func TestAdminViewerPassword_RejectsShort(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)
	resp := postAdminViewerJSON(t, env, "/a/viewers/"+testViewerMAC+"/password", map[string]any{
		"new_password": "1234567",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAdminViewerPassword_RejectsOnESPViewer(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	adoptESPForTest(t, env, espTestMAC, "Wohnung ESP Pw")
	resp := postAdminViewerJSON(t, env, "/a/viewers/"+espTestMAC+"/password", map[string]any{
		"new_password": "irrelevant1234",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (ESP hat kein Passwort)", resp.StatusCode)
	}
}

// ---------- ESP token regenerate ----------

func TestAdminViewerRegenerateToken_ReturnsClearTextOnce(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	adoptESPForTest(t, env, espTestMAC, "Wohnung ESP Tok")

	resp := postAdminViewerJSON(t, env, "/a/viewers/"+espTestMAC+"/regenerate-token", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}
	var out adminViewerTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.OK {
		t.Errorf("ok = false")
	}
	if out.NewToken == "" {
		t.Errorf("new_token leer")
	}
	if out.MAC != espTestMAC {
		t.Errorf("mac echo = %q", out.MAC)
	}
	// Token funktioniert als Bearer.
	mac, err := env.viewerMgr.LookupDeviceMACByToken(context.Background(), out.NewToken)
	if err != nil {
		t.Fatalf("LookupDeviceMACByToken: %v", err)
	}
	if mac != espTestMAC {
		t.Errorf("lookup -> %q, want %q", mac, espTestMAC)
	}
}

func TestAdminViewerRegenerateToken_RejectsOnWebViewer(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)
	resp := postAdminViewerJSON(t, env, "/a/viewers/"+testViewerMAC+"/regenerate-token", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// ---------- Detail-Page Markup ----------

// detailPageMarkup returns the HTML INSIDE <main>...</main> minus
// the trailing <script>-Block. Lets markup-only assertions ignore
// admin-nav-Scripts (vor <main>) und das History/Edit-JS (am Ende).
// Both JS-Bereiche enthalten Selector-Strings die in beiden
// {{if eq .Type ...}}-Branches vorkommen und das naive
// contains(body, "...")-Pattern verfaelschen wuerden.
func detailPageMarkup(body string) string {
	mainStart := indexOf(body, "<main")
	if mainStart < 0 {
		return body
	}
	scriptInMain := indexOfFrom(body, "<script", mainStart)
	mainEnd := indexOfFrom(body, "</main>", mainStart)
	end := mainEnd
	if scriptInMain >= 0 && (mainEnd < 0 || scriptInMain < mainEnd) {
		end = scriptInMain
	}
	if end < 0 {
		return body[mainStart:]
	}
	return body[mainStart:end]
}

// indexOfFrom liefert die erste Position von needle ab offset, -1
// wenn nicht gefunden.
func indexOfFrom(haystack, needle string, offset int) int {
	if offset < 0 || offset >= len(haystack) {
		return -1
	}
	idx := indexOf(haystack[offset:], needle)
	if idx < 0 {
		return -1
	}
	return idx + offset
}

func TestAdminViewerDetail_WebShowsPasswordSection(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)
	resp, err := env.client.Get(env.ts.URL + "/a/viewers/" + testViewerMAC)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	markup := detailPageMarkup(readBody(t, resp))

	if !contains(markup, `data-action="edit-stammdaten"`) {
		t.Errorf("Edit-Stammdaten-Button fehlt")
	}
	if !contains(markup, `data-action="reset-password"`) {
		t.Errorf("Password-Reset-Button fehlt (Web-Viewer)")
	}
	if !contains(markup, `id="password-modal"`) {
		t.Errorf("Password-Modal fehlt (Web-Viewer)")
	}
	if contains(markup, `data-action="regen-token"`) {
		t.Errorf("Token-Regen-Button auf Web-Viewer sichtbar (sollte nur bei ESP)")
	}
	if contains(markup, `id="token-confirm-modal"`) {
		t.Errorf("Token-Confirm-Modal auf Web-Viewer sichtbar (sollte nur bei ESP)")
	}
	if !contains(markup, `id="settings-section"`) {
		t.Errorf("Settings-Section fehlt")
	}
}

func TestAdminViewerDetail_ESPShowsTokenSection(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	adoptESPForTest(t, env, espTestMAC, "Wohnung ESP Detail")
	resp, err := env.client.Get(env.ts.URL + "/a/viewers/" + espTestMAC)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	markup := detailPageMarkup(readBody(t, resp))

	if !contains(markup, `data-action="regen-token"`) {
		t.Errorf("Token-Regen-Button fehlt (ESP-Viewer)")
	}
	if !contains(markup, `id="token-confirm-modal"`) {
		t.Errorf("Token-Confirm-Modal fehlt (ESP-Viewer)")
	}
	if !contains(markup, `id="token-display-modal"`) {
		t.Errorf("Token-Display-Modal fehlt (ESP-Viewer)")
	}
	if contains(markup, `data-action="reset-password"`) {
		t.Errorf("Password-Button auf ESP-Viewer sichtbar (sollte nur bei Web)")
	}
	if contains(markup, `id="password-modal"`) {
		t.Errorf("Password-Modal auf ESP-Viewer sichtbar (sollte nur bei Web)")
	}
	// ESP-spezifische Settings-Fields.
	if !contains(markup, `name="brightness_idle"`) {
		t.Errorf("Brightness-Slider fehlt (ESP-Viewer)")
	}
	if !contains(markup, `name="screen_off_after_sec"`) {
		t.Errorf("Screen-Off-Radios fehlen (ESP-Viewer)")
	}
	if !contains(markup, `name="language"`) {
		t.Errorf("Sprach-Radios fehlen (ESP-Viewer)")
	}
}

func TestAdminViewerDetail_WebHidesESPSettings(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)
	resp, err := env.client.Get(env.ts.URL + "/a/viewers/" + testViewerMAC)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	markup := detailPageMarkup(readBody(t, resp))

	for _, espField := range []string{
		`name="brightness_idle"`,
		`name="screen_off_after_sec"`,
		`name="language"`,
	} {
		if contains(markup, espField) {
			t.Errorf("ESP-Settings-Field %q ist im Web-Viewer-Markup sichtbar", espField)
		}
	}
	// Web-Viewer hat NICHT screen_off als idle_view_mode Option.
	if contains(markup, `value="screen_off"`) {
		t.Errorf("idle_view_mode=screen_off Option im Web-Viewer sichtbar (sollte nur bei ESP)")
	}
}

func TestAdminViewerRegenerateToken_InvalidatesOldToken(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	oldToken := adoptESPForTest(t, env, espTestMAC, "Wohnung ESP Inv")

	resp := postAdminViewerJSON(t, env, "/a/viewers/"+espTestMAC+"/regenerate-token", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out adminViewerTokenResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.NewToken == oldToken {
		t.Fatalf("new token equals old token - regen broken")
	}
	// Alter Bearer-Token wird zurueckgewiesen.
	_, err := env.viewerMgr.LookupDeviceMACByToken(context.Background(), oldToken)
	if err == nil {
		t.Errorf("old token still resolves to viewer; rotation failed")
	}
}

