package httpserver

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// loginAdmin runs the setup-then-login flow against the test
// server and returns the post-login session cookie value.
func loginAdmin(t *testing.T, env *testEnv, username, password string) {
	t.Helper()
	// Setup-Form.
	form := url.Values{}
	form.Set("setup", "1")
	form.Set("username", username)
	form.Set("password", password)
	form.Set("password_confirm", password)
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

// ---------- Login + setup ----------

func TestAdminLogin_GetShowsSetupWhenNoAdmin(t *testing.T) {
	env := newTestServer(t)
	resp, err := env.client.Get(env.ts.URL + "/a/login")
	if err != nil {
		t.Fatalf("GET /a/login: %v", err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, "Erstmaliger Setup") {
		t.Errorf("body missing setup marker, got: %s", body[:min(200, len(body))])
	}
	if !strings.Contains(body, "Admin anlegen") {
		t.Errorf("body missing setup button")
	}
}

func TestAdminLogin_SetupCreatesAdminAndLogsIn(t *testing.T) {
	env := newTestServer(t)
	form := url.Values{}
	form.Set("setup", "1")
	form.Set("username", "saschsa")
	form.Set("password", "lange-langes-passwort")
	form.Set("password_confirm", "lange-langes-passwort")
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
	// admin user now exists in DB
	ok, err := env.adminSvc.Exists(context.Background())
	if err != nil || !ok {
		t.Errorf("Exists after setup = %v err=%v, want true nil", ok, err)
	}
	// cookie present
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

func TestAdminLogin_SetupRejectsMismatch(t *testing.T) {
	env := newTestServer(t)
	form := url.Values{}
	form.Set("setup", "1")
	form.Set("username", "saschsa")
	form.Set("password", "abcdefgh")
	form.Set("password_confirm", "different")
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/a/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /a/login: %v", err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, "stimmen nicht ueberein") {
		t.Errorf("missing mismatch error in body")
	}
}

func TestAdminLogin_AfterSetupGetShowsLoginNotSetup(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, "saschsa", "lange-langes-passwort")
	// fresh client (no cookie) to avoid auto-redirect.
	resp, err := http.Get(env.ts.URL + "/a/login")
	if err != nil {
		t.Fatalf("GET /a/login: %v", err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if strings.Contains(body, "Erstmaliger Setup") {
		t.Error("login page still shows setup mode after admin exists")
	}
	if !strings.Contains(body, "Anmeldung erforderlich") {
		t.Errorf("missing login marker")
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
	if !strings.Contains(body, "Willkommen, saschsa") {
		t.Errorf("missing personalized greeting")
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
	if !strings.Contains(body, "API Token") {
		t.Errorf("missing API Token field")
	}
}

func TestAdminSettings_PostStoresEncrypted(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, "saschsa", "lange-langes-passwort")
	form := url.Values{}
	form.Set("action", "save")
	form.Set("base_url", "https://example.com:12445")
	form.Set("token", "secret-token-abcdef")
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/a/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /a/settings: %v", err)
	}
	resp.Body.Close()

	// plaintext base url stored
	gotURL, err := env.platformCfg.Get(context.Background(), "ua_api_base_url")
	if err != nil {
		t.Fatalf("Get base_url: %v", err)
	}
	if gotURL != "https://example.com:12445" {
		t.Errorf("base_url = %q", gotURL)
	}
	// token stored encrypted; decrypt yields plaintext
	gotToken, err := env.platformCfg.GetSecret(context.Background(), "ua_api_token")
	if err != nil {
		t.Fatalf("GetSecret token: %v", err)
	}
	if gotToken != "secret-token-abcdef" {
		t.Errorf("token = %q", gotToken)
	}
	// raw DB row must not contain the plaintext token
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

// ---------- Mocks CRUD ----------

func TestAdminMocks_ListEmpty(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, "saschsa", "lange-langes-passwort")
	resp, err := env.client.Get(env.ts.URL + "/a/mocks")
	if err != nil {
		t.Fatalf("GET /a/mocks: %v", err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, "Noch keine Mock-Viewer") {
		t.Errorf("missing empty-state marker")
	}
	if !strings.Contains(body, "Neuen Mock-Viewer anlegen") {
		t.Errorf("missing create form")
	}
}

// htmlIDWithColonRE matches any HTML id="..." attribute whose
// value contains a colon. Colons in IDs are legal HTML but
// blow up querySelectorAll because ":" starts a CSS pseudo
// class. htmx hits that path on every hx-target, so the
// admin mocks UI needs colon-free row IDs.
var htmlIDWithColonRE = regexp.MustCompile(`(?i)\bid\s*=\s*"[^"]*:[^"]*"`)

// htmxSelectorWithColonRE catches the same hazard in hx-target
// (or any other htmx attribute that takes a CSS selector).
var htmxSelectorWithColonRE = regexp.MustCompile(`(?i)\bhx-(target|swap-oob|trigger|select)\s*=\s*"[^"]*#[^"]*:[^"]*"`)

func TestMocksRow_RenderedIDsHaveNoColons(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, "saschsa", "lange-langes-passwort")

	form := url.Values{}
	form.Set("name", "Colon Probe")
	form.Set("mac", "0c:ea:14:0a:78:06")
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/a/mocks", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /a/mocks: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create status = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)

	if m := htmlIDWithColonRE.FindString(body); m != "" {
		t.Errorf("rendered row has HTML id with colon (breaks CSS selectors): %s", m)
	}
	if m := htmxSelectorWithColonRE.FindString(body); m != "" {
		t.Errorf("rendered row has htmx selector with colon (breaks querySelectorAll): %s", m)
	}
	// The expected colon-free pattern must be present.
	if !strings.Contains(body, `id="mock-row-0cea140a7806"`) {
		t.Errorf("expected colon-free id mock-row-0cea140a7806 in body; got: %s", body)
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
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "0c:ea:14:de:ad:be") {
		t.Errorf("partial response missing MAC")
	}
	if !strings.Contains(body, "Smoke Test Mock") {
		t.Errorf("partial response missing name")
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
	// mac empty -> auto generate
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/a/mocks", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /a/mocks: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "0c:ea:14:") {
		t.Errorf("auto-generated MAC missing Ubiquiti OUI prefix")
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

func TestAdminMocksMagicLink_RequiresUserBinding(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, "saschsa", "lange-langes-passwort")

	createForm := url.Values{}
	createForm.Set("name", "No Binding")
	createForm.Set("mac", "0c:ea:14:55:55:55")
	createReq, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/a/mocks", strings.NewReader(createForm.Encode()))
	createReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := env.client.Do(createReq); err != nil {
		t.Fatalf("create mock: %v", err)
	}

	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/a/mocks/0c:ea:14:55:55:55/magic-link", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST magic-link: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "keinem Mieter") {
		t.Errorf("body missing tenant-binding hint, got: %s", body)
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

	bindForm := url.Values{}
	bindForm.Set("ua_user_id", "ua-user-mueller")
	bindReq, _ := http.NewRequest(http.MethodPut,
		env.ts.URL+"/a/mocks/0c:ea:14:77:77:77/binding",
		strings.NewReader(bindForm.Encode()))
	bindReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := env.client.Do(bindReq); err != nil {
		t.Fatalf("bind user: %v", err)
	}

	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/a/mocks/0c:ea:14:77:77:77/magic-link", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST magic-link: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Familie Mueller") {
		t.Errorf("body missing mock name, got: %s", body)
	}
	if !strings.Contains(body, "/m/login?t=") {
		t.Errorf("body missing /m/login?t= URL, got: %s", body)
	}

	// Pluck the token out of the rendered HTML and verify it
	// actually consumes through to the bound ua_user_id.
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
	ua, err := env.magic.Consume(context.Background(), token)
	if err != nil {
		t.Fatalf("consume token: %v", err)
	}
	if ua != "ua-user-mueller" {
		t.Errorf("consume returned ua=%q, want ua-user-mueller", ua)
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

func TestAdminMocks_BindingUpdate(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, "saschsa", "lange-langes-passwort")

	form := url.Values{}
	form.Set("name", "Binding Test")
	form.Set("mac", "0c:ea:14:22:22:22")
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/a/mocks", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := env.client.Do(req); err != nil {
		t.Fatalf("create: %v", err)
	}

	bindForm := url.Values{}
	bindForm.Set("ua_user_id", "ua-user-xyz")
	bindReq, _ := http.NewRequest(http.MethodPut,
		env.ts.URL+"/a/mocks/0c:ea:14:22:22:22/binding",
		strings.NewReader(bindForm.Encode()))
	bindReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := env.client.Do(bindReq)
	if err != nil {
		t.Fatalf("PUT binding: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	got, err := env.mockMgr.LookupUserByMAC(context.Background(), "0c:ea:14:22:22:22")
	if err != nil {
		t.Fatalf("LookupUserByMAC: %v", err)
	}
	if got != "ua-user-xyz" {
		t.Errorf("LookupUserByMAC = %q, want ua-user-xyz", got)
	}
}

// ---------- Users (no UA configured) ----------

func TestAdminUsers_RequiresUAConfig(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, "saschsa", "lange-langes-passwort")
	resp, err := env.client.Get(env.ts.URL + "/a/users")
	if err != nil {
		t.Fatalf("GET /a/users: %v", err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, "UA-API nicht konfiguriert") {
		t.Errorf("missing UA-not-configured marker")
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

	// follow-up request must be unauthenticated again
	resp2, err := env.client.Get(env.ts.URL + "/a/")
	if err != nil {
		t.Fatalf("GET /a/ after logout: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusSeeOther {
		t.Errorf("dashboard after logout = %d, want 303", resp2.StatusCode)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
