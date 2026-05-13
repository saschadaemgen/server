package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

const adminTestUser = "saschsa"
const adminTestPassword = "lange-langes-passwort-1234"

// loginAdmin posts a single (username, password) form to /a/login.
// Saison 13-02-FIX4-a: rate limiter, audit log und Argon2id sind
// transparent fuer den Caller; das Verhalten der ersten POST
// (first-run-setup oder Login) bleibt gleich.
func loginAdmin(t *testing.T, env *testEnv, username, password string) {
	t.Helper()
	form := url.Values{}
	form.Set("username", username)
	form.Set("password", password)
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/a/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("setup POST /a/login: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("setup login status = %d, want 303", resp.StatusCode)
	}
}

// ---------- Login + first-run setup ----------

func TestAdminLogin_GetRendersLibraryForm(t *testing.T) {
	env := newTestServer(t)
	resp, err := env.client.Get(env.ts.URL + "/a/login")
	if err != nil {
		t.Fatalf("GET /a/login: %v", err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, `name="username"`) {
		t.Errorf("login form missing username input")
	}
	if !strings.Contains(body, `name="password"`) {
		t.Errorf("login form missing password input")
	}
}

func TestAdminLogin_FirstPostSetsUpAdminAndLogsIn(t *testing.T) {
	env := newTestServer(t)
	form := url.Values{}
	form.Set("username", adminTestUser)
	form.Set("password", adminTestPassword)
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/a/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /a/login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/a/" {
		t.Errorf("Location = %q, want /a/", loc)
	}
	ok, err := env.adminSvc.Exists(context.Background())
	if err != nil || !ok {
		t.Errorf("Exists after first-post = %v err=%v, want true nil", ok, err)
	}
	var found bool
	for _, c := range resp.Cookies() {
		if c.Name == AdminSessionCookieName && c.Value != "" {
			found = true
			break
		}
	}
	if !found {
		t.Error("admin session cookie missing after setup")
	}
}

func TestAdminLogin_WrongPassword(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	form := url.Values{}
	form.Set("username", adminTestUser)
	form.Set("password", "wrong-password-1234")
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/a/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /a/login: %v", err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, "ungueltig") {
		t.Errorf("missing invalid-credentials marker in body, got: %s", body)
	}
}

// ---------- Dashboard guard ----------

func TestAdminDashboard_RequiresSession(t *testing.T) {
	env := newTestServer(t)
	resp, err := env.client.Get(env.ts.URL + "/a/")
	if err != nil {
		t.Fatalf("GET /a/: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/a/login" {
		t.Errorf("Location = %q, want /a/login", loc)
	}
}

func TestAdminDashboard_HappyPath(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	resp, err := env.client.Get(env.ts.URL + "/a/")
	if err != nil {
		t.Fatalf("GET /a/: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Dashboard") {
		t.Errorf("missing Dashboard heading")
	}
	if !strings.Contains(body, adminTestUser) {
		t.Errorf("missing admin name in nav")
	}
}

// ---------- Settings ----------

func TestAdminSettings_Get(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	resp, err := env.client.Get(env.ts.URL + "/a/settings")
	if err != nil {
		t.Fatalf("GET /a/settings: %v", err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, "Controller-URL") {
		t.Errorf("missing UA controller url field")
	}
	if !strings.Contains(body, "Einstellungen") {
		t.Errorf("missing settings heading")
	}
}

func TestAdminSettings_PostStoresEncrypted(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	form := url.Values{}
	form.Set("ua_controller_url", "https://example.com:12445")
	form.Set("token", "secret-token-abcdef")
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/a/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /a/settings: %v", err)
	}
	resp.Body.Close()

	gotURL, err := env.platformCfg.Get(context.Background(), "ua_api_base_url")
	if err != nil {
		t.Fatalf("Get base_url: %v", err)
	}
	if gotURL != "https://example.com:12445" {
		t.Errorf("base_url = %q", gotURL)
	}
	gotToken, err := env.platformCfg.GetSecret(context.Background(), "ua_api_token")
	if err != nil {
		t.Fatalf("GetSecret token: %v", err)
	}
	if gotToken != "secret-token-abcdef" {
		t.Errorf("token = %q", gotToken)
	}
}

// ---------- Web-Viewer-CRUD ----------

func TestAdminWebViewers_ListEmpty(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	resp, err := env.client.Get(env.ts.URL + "/a/web-viewers")
	if err != nil {
		t.Fatalf("GET /a/web-viewers: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Web-Viewer") {
		t.Errorf("missing Web-Viewer heading")
	}
	if !strings.Contains(body, "Noch keine Web-Viewer") {
		t.Errorf("empty-state hint missing")
	}
}

func TestAdminWebViewers_CreateReturnsCredentials(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	form := url.Values{}
	form.Set("name", "Familie Mueller")
	form.Set("mac", "0c:ea:14:de:ad:be")
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/a/web-viewers", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /a/web-viewers: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var c credentialsResponse
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if c.Username == "" || c.Password == "" {
		t.Errorf("response missing credentials: %+v", c)
	}
	// S13-02-FIX4-a-HOTFIX1: Login-URL ist jetzt nackt (kein
	// ?u= / ?p= mehr); das war ein Sicherheits-Anti-Pattern.
	if !strings.HasSuffix(c.LoginURL, "/einloggen") {
		t.Errorf("login_url should be plain /einloggen, got: %q", c.LoginURL)
	}
	if strings.Contains(c.LoginURL, "?u=") || strings.Contains(c.LoginURL, "&p=") {
		t.Errorf("login_url leaks credentials: %q", c.LoginURL)
	}
	if !strings.HasPrefix(c.QRSVG, "<svg") {
		t.Errorf("qr_svg missing svg root")
	}

	infos, _ := env.mockMgr.ListViewers(context.Background())
	if len(infos) != 1 {
		t.Fatalf("ListViewers len = %d, want 1", len(infos))
	}
	if !infos[0].HasPassword {
		t.Errorf("created viewer has no password set")
	}
}

func TestAdminWebViewers_ResetPassword(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/a/web-viewers/"+testViewerMAC+"/reset-pw", nil)
	req.Header.Set("Accept", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST reset-pw: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var c credentialsResponse
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if c.Password == testViewerPassword {
		t.Error("reset returned same password (RNG broken?)")
	}
	// Old password muss jetzt nicht mehr funktionieren.
	resp2 := env.loginViewer(t, testViewerUsername, testViewerPassword)
	defer resp2.Body.Close()
	if resp2.StatusCode == http.StatusSeeOther {
		t.Errorf("old password still works after reset")
	}
}

func TestAdminWebViewers_Delete(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	req, _ := http.NewRequest(http.MethodDelete,
		env.ts.URL+"/a/web-viewers/"+testViewerMAC, nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	infos, _ := env.mockMgr.ListViewers(context.Background())
	if len(infos) != 0 {
		t.Errorf("ListViewers len = %d, want 0", len(infos))
	}
}

// ---------- Placeholder-Pages ----------

func TestAdminPlaceholders_Render(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	cases := []struct {
		path string
		want string
	}{
		{"/a/esp-viewers", "ESP-Viewer"},
		{"/a/users", "Benutzer"},
		{"/a/esp-pager", "ESP-Pager"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			resp, err := env.client.Get(env.ts.URL + c.path)
			if err != nil {
				t.Fatalf("GET %s: %v", c.path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200", resp.StatusCode)
			}
			body := readBody(t, resp)
			if !strings.Contains(body, c.want) {
				t.Errorf("body missing %q", c.want)
			}
		})
	}
}

// ---------- Magic-Link routes are gone ----------

func TestMagicLinkRoutes_AreGone(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	for _, path := range []string{
		"/einloggen/login?t=abcdef",
		"/a/mocks",
		"/a/mocks/0c:ea:14:42:42:42/magic-link",
	} {
		resp, err := env.client.Get(env.ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s returned 200, want 404 / 405 (route should be gone)", path)
		}
	}
}

// ---------- Logout ----------

func TestAdminLogout_RevokesSessionAndClearsCookie(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/a/logout", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /a/logout: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/a/login" {
		t.Errorf("Location = %q, want /a/login", loc)
	}

	resp2, err := env.client.Get(env.ts.URL + "/a/")
	if err != nil {
		t.Fatalf("GET /a/ after logout: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusSeeOther {
		t.Errorf("dashboard after logout = %d, want 303", resp2.StatusCode)
	}
}
