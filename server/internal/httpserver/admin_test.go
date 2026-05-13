package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// loginAdmin posts a single (username, password) form to /a/login.
// Saison 13-02-FIX3: the previous "first-run setup" flow is gone;
// the very first valid POST creates the admin row and logs in.
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
	if !strings.Contains(body, "Anmelden") {
		t.Errorf("login form missing submit label")
	}
}

func TestAdminLogin_FirstPostSetsUpAdminAndLogsIn(t *testing.T) {
	env := newTestServer(t)
	form := url.Values{}
	form.Set("username", "saschsa")
	form.Set("password", "lange-langes-passwort")
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
	loginAdmin(t, env, "saschsa", "lange-langes-passwort")

	form := url.Values{}
	form.Set("username", "saschsa")
	form.Set("password", "wrong-password")
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/a/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /a/login: %v", err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, "ungueltig") {
		t.Errorf("missing invalid-credentials marker in body")
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
	loginAdmin(t, env, "saschsa", "lange-langes-passwort")
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
	if !strings.Contains(body, "saschsa") {
		t.Errorf("missing admin name in nav (got no occurrence of \"saschsa\")")
	}
}

// ---------- Settings ----------

func TestAdminSettings_Get(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, "saschsa", "lange-langes-passwort")
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
	loginAdmin(t, env, "saschsa", "lange-langes-passwort")
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
	var enc string
	if err := env.d.QueryRow(
		`SELECT value_encrypted FROM platform_config WHERE key = ?`, "ua_api_token",
	).Scan(&enc); err != nil {
		t.Fatalf("query raw: %v", err)
	}
	if strings.Contains(enc, "secret-token-abcdef") {
		t.Error("plaintext token leaked into value_encrypted row")
	}
}

// ---------- Mocks ----------

func TestAdminMocks_ListRendersDeviceTable(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, "saschsa", "lange-langes-passwort")
	resp, err := env.client.Get(env.ts.URL + "/a/mocks")
	if err != nil {
		t.Fatalf("GET /a/mocks: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Mock-Tools") {
		t.Errorf("missing Mock-Tools heading")
	}
}

func TestAdminMocks_CreateHappyPath(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, "saschsa", "lange-langes-passwort")

	form := url.Values{}
	form.Set("name", "Smoke Test Mock")
	form.Set("mac", "0c:ea:14:de:ad:be")
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/a/mocks", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /a/mocks: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303 (redirect after create)", resp.StatusCode)
	}
	infos, _ := env.mockMgr.ListViewers(context.Background())
	if len(infos) != 1 {
		t.Errorf("ListViewers len = %d, want 1", len(infos))
	}
}

func TestAdminMocks_CreateGeneratesMACWhenEmpty(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, "saschsa", "lange-langes-passwort")

	form := url.Values{}
	form.Set("name", "Auto MAC")
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/a/mocks", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /a/mocks: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", resp.StatusCode)
	}
	infos, _ := env.mockMgr.ListViewers(context.Background())
	if len(infos) != 1 {
		t.Fatalf("ListViewers len = %d, want 1", len(infos))
	}
	if !strings.HasPrefix(infos[0].MAC, "0c:ea:14:") {
		t.Errorf("auto-generated MAC missing Ubiquiti OUI prefix: %s", infos[0].MAC)
	}
}

func TestAdminMocks_CreateRejectsBadMAC(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, "saschsa", "lange-langes-passwort")
	form := url.Values{}
	form.Set("name", "Bad MAC")
	form.Set("mac", "not-a-mac")
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/a/mocks", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /a/mocks: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAdminMocks_Delete(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, "saschsa", "lange-langes-passwort")

	form := url.Values{}
	form.Set("name", "Delete Me")
	form.Set("mac", "0c:ea:14:11:11:11")
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/a/mocks", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := env.client.Do(req); err != nil {
		t.Fatalf("create: %v", err)
	}

	delReq, _ := http.NewRequest(http.MethodDelete, env.ts.URL+"/a/mocks/0c:ea:14:11:11:11", nil)
	resp, err := env.client.Do(delReq)
	if err != nil {
		t.Fatalf("DELETE /a/mocks: %v", err)
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

// ---------- Magic-Link generator ----------

func TestAdminMocksMagicLink_RequiresAdminSession(t *testing.T) {
	env := newTestServer(t)
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/a/mocks/0c:ea:14:11:11:11/magic-link", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", resp.StatusCode)
	}
}

func TestAdminMocksMagicLink_NotFound(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, "saschsa", "lange-langes-passwort")
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/a/mocks/0c:ea:14:99:99:99/magic-link", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestAdminMocksMagicLink_HappyPath(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, "saschsa", "lange-langes-passwort")

	createForm := url.Values{}
	createForm.Set("name", "Familie Mueller")
	createForm.Set("mac", "0c:ea:14:77:77:77")
	createReq, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/a/mocks", strings.NewReader(createForm.Encode()))
	createReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := env.client.Do(createReq); err != nil {
		t.Fatalf("create mock: %v", err)
	}

	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/a/mocks/0c:ea:14:77:77:77/magic-link", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST magic-link: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Familie Mueller") {
		t.Errorf("body missing mock name, got: %s", body)
	}
	if !strings.Contains(body, "/m/login?t=") {
		t.Errorf("body missing /m/login?t= URL, got: %s", body)
	}

	const marker = "/m/login?t="
	idx := strings.Index(body, marker)
	if idx < 0 {
		t.Fatal("token not extractable from response")
	}
	rest := body[idx+len(marker):]
	end := strings.IndexAny(rest, "\"' \t\r\n<")
	if end < 0 {
		t.Fatal("token end marker missing")
	}
	token := rest[:end]
	mac, err := env.magic.Consume(context.Background(), token)
	if err != nil {
		t.Fatalf("consume token: %v", err)
	}
	if mac != "0c:ea:14:77:77:77" {
		t.Errorf("consume returned mac=%q, want 0c:ea:14:77:77:77", mac)
	}
}

// TestAdminMocksMagicLink_JSON covers the Saison 13-02-FIX3b
// glue path: with Accept: application/json the magic-link
// handler returns {url, tenant_name, expires_in_hours, qr_svg}
// instead of the full modal HTML fragment, and the alias
// route /a/users/{mac}/magic-link reaches the same handler.
func TestAdminMocksMagicLink_JSON(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, "saschsa", "lange-langes-passwort")

	createForm := url.Values{}
	createForm.Set("name", "Familie Mueller")
	createForm.Set("mac", "0c:ea:14:33:33:33")
	createReq, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/a/mocks", strings.NewReader(createForm.Encode()))
	createReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := env.client.Do(createReq); err != nil {
		t.Fatalf("create mock: %v", err)
	}

	for _, path := range []string{
		"/a/mocks/0c:ea:14:33:33:33/magic-link",
		"/a/users/0c:ea:14:33:33:33/magic-link",
	} {
		req, _ := http.NewRequest(http.MethodPost, env.ts.URL+path, nil)
		req.Header.Set("Accept", "application/json")
		resp, err := env.client.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d, body=%s", path, resp.StatusCode, body)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s content-type = %q, want application/json", path, ct)
		}
		var payload struct {
			URL            string `json:"url"`
			TenantName     string `json:"tenant_name"`
			ExpiresInHours int    `json:"expires_in_hours"`
			QRSVG          string `json:"qr_svg"`
		}
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			t.Fatalf("%s: decode json: %v (body=%s)", path, err, body)
		}
		if payload.TenantName != "Familie Mueller" {
			t.Errorf("%s: tenant_name = %q", path, payload.TenantName)
		}
		if !strings.Contains(payload.URL, "/m/login?t=") {
			t.Errorf("%s: url missing magic-link prefix: %q", path, payload.URL)
		}
		if payload.ExpiresInHours != 24 {
			t.Errorf("%s: expires_in_hours = %d, want 24", path, payload.ExpiresInHours)
		}
		if !strings.HasPrefix(payload.QRSVG, "<svg") {
			t.Errorf("%s: qr_svg missing svg root: %q", path, payload.QRSVG)
		}
	}
}

func TestAdminMocksMagicLink_RejectsBadMAC(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, "saschsa", "lange-langes-passwort")
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/a/mocks/not-a-mac/magic-link", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// ---------- Users page ----------
//
// Saison 13-02-FIX3: /a/users renders regardless of whether the
// UA-API is configured. It now surfaces our mock_viewers as
// tenant rows; UA-Developer-API data is joined where available.

func TestAdminUsers_RendersTenantTable(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, "saschsa", "lange-langes-passwort")
	resp, err := env.client.Get(env.ts.URL + "/a/users")
	if err != nil {
		t.Fatalf("GET /a/users: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Mieter") {
		t.Errorf("missing Mieter heading")
	}
}

// ---------- Logout ----------

func TestAdminLogout_RevokesSessionAndClearsCookie(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, "saschsa", "lange-langes-passwort")

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
