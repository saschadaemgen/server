package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"carvilon.local/server/internal/access"
	"carvilon.local/server/internal/doorhistory"
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
	if c.Password == "" {
		t.Errorf("response missing password: %+v", c)
	}
	if c.Name == "" {
		t.Errorf("response missing name: %+v", c)
	}
	// S13-02-FIX4-a-HOTFIX1: Login-URL ist jetzt nackt (kein
	// ?u= / ?p= mehr); das war ein Sicherheits-Anti-Pattern.
	if !strings.HasSuffix(c.LoginURL, "/login") {
		t.Errorf("login_url should be plain /login, got: %q", c.LoginURL)
	}
	if strings.Contains(c.LoginURL, "?u=") || strings.Contains(c.LoginURL, "&p=") {
		t.Errorf("login_url leaks credentials: %q", c.LoginURL)
	}
	if !strings.HasPrefix(c.QRSVG, "<svg") {
		t.Errorf("qr_svg missing svg root")
	}

	infos, _ := env.viewerMgr.ListViewers(context.Background())
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
	resp2 := env.loginViewer(t, testViewerLogin, testViewerPassword)
	defer resp2.Body.Close()
	if resp2.StatusCode == http.StatusSeeOther {
		t.Errorf("old password still works after reset")
	}
}

// TestPasswordReset_InvalidatesSessions ist der HOTFIX3-Acceptance-
// Test fuer Auto-Logout: nach Reset-PW darf KEIN viewer_session-
// Eintrag des Viewers in der DB stehenbleiben. Login-Cookie des
// vorher angemeldeten Mieters muss also wirkungslos sein.
func TestPasswordReset_InvalidatesSessions(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	// Mieter loggt sich ein, hat damit einen viewer_sessions-Eintrag.
	respLogin := env.loginViewer(t, testViewerLogin, testViewerPassword)
	respLogin.Body.Close()
	if respLogin.StatusCode != http.StatusSeeOther {
		t.Fatalf("seed login failed status=%d", respLogin.StatusCode)
	}
	var before int
	if err := env.d.QueryRow(
		`SELECT COUNT(*) FROM viewer_sessions WHERE viewer_mac = ?`, testViewerMAC,
	).Scan(&before); err != nil {
		t.Fatalf("count before: %v", err)
	}
	if before == 0 {
		t.Fatalf("seed login did not create a viewer_sessions row")
	}

	// Admin macht Reset-PW.
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/a/web-viewers/"+testViewerMAC+"/reset-pw", nil)
	req.Header.Set("Accept", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("reset-pw: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reset-pw status = %d, want 200", resp.StatusCode)
	}

	// Sessions des Viewers sollten weg sein.
	var after int
	if err := env.d.QueryRow(
		`SELECT COUNT(*) FROM viewer_sessions WHERE viewer_mac = ?`, testViewerMAC,
	).Scan(&after); err != nil {
		t.Fatalf("count after: %v", err)
	}
	if after != 0 {
		t.Errorf("viewer_sessions after reset = %d, want 0 (auto-logout missing)", after)
	}
}

// TestDashboard_RealNumbers seedet Viewer + Sessions + door_events
// und prueft dass die vier KPI-Karten echte Zahlen aus der DB
// rendern (HOTFIX3 macht Schluss mit Fake-Werten).
func TestDashboard_RealNumbers(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewerAs(t, "0c:ea:14:33:33:01", "Viewer A", "TestPw-1234567X")
	env.seedViewerAs(t, "0c:ea:14:33:33:02", "Viewer B", "TestPw-1234567X")

	// Ein Login -> eine aktive Session
	respLogin := env.loginViewer(t, "viewer-a", "TestPw-1234567X")
	respLogin.Body.Close()

	// door_events seeden (Mitte des Tages + alt)
	now := time.Now()
	if _, err := env.history.Insert(context.Background(),
		doorhistory.Event{
			ViewerMAC: "0c:ea:14:33:33:01", EventType: doorhistory.TypeDoorbellStart,
			OccurredAt: now.Add(-2 * time.Hour),
		}, nil); err != nil {
		t.Fatalf("insert event today: %v", err)
	}
	if _, err := env.history.Insert(context.Background(),
		doorhistory.Event{
			ViewerMAC: "0c:ea:14:33:33:02", EventType: doorhistory.TypeDoorbellStart,
			OccurredAt: now.Add(-3 * 24 * time.Hour),
		}, nil); err != nil {
		t.Fatalf("insert event 3d: %v", err)
	}
	if _, err := env.history.Insert(context.Background(),
		doorhistory.Event{
			ViewerMAC: "0c:ea:14:33:33:01", EventType: doorhistory.TypeDoorbellStart,
			OccurredAt: now.Add(-10 * 24 * time.Hour),
		}, nil); err != nil {
		t.Fatalf("insert event 10d: %v", err)
	}

	resp, err := env.client.Get(env.ts.URL + "/a/")
	if err != nil {
		t.Fatalf("GET /a/: %v", err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)

	if !strings.Contains(body, "Web-Viewer gesamt") {
		t.Errorf("dashboard missing 'Web-Viewer gesamt' label")
	}
	if !strings.Contains(body, "Klingel-Events heute") {
		t.Errorf("dashboard missing 'Klingel-Events heute' label")
	}
	if !strings.Contains(body, "Klingel-Events 7 Tage") {
		t.Errorf("dashboard missing 'Klingel-Events 7 Tage' label")
	}
	if !strings.Contains(body, "Aktive Sessions") {
		t.Errorf("dashboard missing 'Aktive Sessions' label")
	}
	// Web-Viewer-Anzahl 2
	if !strings.Contains(body, `<div class="stat-card-value">2</div>`) {
		t.Errorf("dashboard does not render WebViewersTotal=2; body sample:\n%s", body[:min(2000, len(body))])
	}
	// Recent-Events-Liste hat Viewer-A-Eintrag
	if !strings.Contains(body, "Viewer A") {
		t.Errorf("recent events list missing 'Viewer A'")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------- Saison 14-XX admin-edit config.changed broadcasts ----------

func TestAdminWebViewers_RenameBroadcastsConfigChanged(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	sub, cleanup := env.hub.Subscribe(testViewerMAC)
	defer cleanup()

	form := url.Values{}
	form.Set("name", "Familie Mueller (umbenannt)")
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/a/web-viewers/"+testViewerMAC+"/rename",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST rename: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	select {
	case ev := <-sub.Events:
		if ev.Type != "config.changed" {
			t.Errorf("ev.Type = %q, want config.changed", ev.Type)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("rename did not trigger config.changed")
	}
}

func TestAdminWebViewers_EditBroadcastsConfigChanged(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	sub, cleanup := env.hub.Subscribe(testViewerMAC)
	defer cleanup()

	body, _ := json.Marshal(map[string]any{
		"name":                testViewerName,
		"paired_intercom_mac": "28:70:4e:31:e2:9c",
	})
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/a/web-viewers/"+testViewerMAC+"/edit",
		strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST edit: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}
	select {
	case ev := <-sub.Events:
		if ev.Type != "config.changed" {
			t.Errorf("ev.Type = %q, want config.changed", ev.Type)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("paired-intercom edit did not trigger config.changed")
	}
}

func TestAdminESPViewers_RenameBroadcastsConfigChanged(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	adoptESPForTest(t, env, espTestMAC, "Wohnung ESP A")

	sub, cleanup := env.hub.Subscribe(espTestMAC)
	defer cleanup()

	form := url.Values{}
	form.Set("name", "Wohnung ESP A (umbenannt)")
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/a/esp-viewers/"+espTestMAC+"/rename",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST esp rename: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	select {
	case ev := <-sub.Events:
		if ev.Type != "config.changed" {
			t.Errorf("ev.Type = %q, want config.changed", ev.Type)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("esp rename did not trigger config.changed")
	}
}

func TestAdminWebViewers_PasswordResetSkipsConfigChanged(t *testing.T) {
	// Password-Reset triggert KEINEN Broadcast - der Browser ist
	// gleich auf /login redirected, kein laufender Subscriber.
	// Trotzdem darf der Handler nicht panicen wenn hub nil-ish ist.
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	sub, cleanup := env.hub.Subscribe(testViewerMAC)
	defer cleanup()

	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/a/web-viewers/"+testViewerMAC+"/reset-pw", nil)
	req.Header.Set("Accept", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST reset-pw: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// Kein Broadcast erwartet - 150ms reicht da der Hub
	// die Subscribe-Channel synchron mit broadcast bedient.
	select {
	case ev := <-sub.Events:
		t.Errorf("password reset triggered config.changed: %+v", ev)
	case <-time.After(150 * time.Millisecond):
		// expected
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
	infos, _ := env.viewerMgr.ListViewers(context.Background())
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

// ---------- Benutzer-Tab (FIX4-b) ----------

func seedAccessUsers(env *testEnv, users ...access.User) {
	env.userStore.users = append(env.userStore.users, users...)
}

func TestUsersList_RendersTable(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	seedAccessUsers(env,
		access.User{ID: "u1", FirstName: "Sascha", LastName: "Daemgen",
			Email: "s@d.com", Status: access.StatusActive},
		access.User{ID: "u2", FirstName: "Anna", LastName: "Mueller",
			Email: "a@m.com", Status: access.StatusDeactivated},
	)
	resp, err := env.client.Get(env.ts.URL + "/a/users")
	if err != nil {
		t.Fatalf("GET /a/users: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	for _, want := range []string{"Sascha Daemgen", "Anna Mueller", "Aktiv", "Inaktiv"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestUsersList_EmptyConfiguredShowsCreateHint(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	resp, err := env.client.Get(env.ts.URL + "/a/users")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, "Noch keine Benutzer angelegt") {
		t.Errorf("empty-state hint missing")
	}
}

func TestUsersList_NotConfiguredShowsHint(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.userStore.configured = false
	resp, err := env.client.Get(env.ts.URL + "/a/users")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, "noch nicht konfiguriert") {
		t.Errorf("not-configured hint missing")
	}
}

func TestUsersList_Pagination(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	for i := 0; i < 5; i++ {
		env.userStore.users = append(env.userStore.users, access.User{
			ID: "u" + string(rune('a'+i)), FirstName: "First", LastName: "Last" + string(rune('A'+i)),
			Status: access.StatusActive,
		})
	}
	resp, err := env.client.Get(env.ts.URL + "/a/users?page=2&size=2")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, "Seite 2 von 3") {
		t.Errorf("expected 'Seite 2 von 3' in body")
	}
}

func TestUsersList_SearchFilter(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	seedAccessUsers(env,
		access.User{ID: "u1", FirstName: "Sascha", LastName: "Daemgen", Status: access.StatusActive},
		access.User{ID: "u2", FirstName: "Anna", LastName: "Mueller", Status: access.StatusActive},
	)
	resp, err := env.client.Get(env.ts.URL + "/a/users?q=mueller")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if strings.Contains(body, "Sascha Daemgen") {
		t.Errorf("search filter leaked non-matching row")
	}
	if !strings.Contains(body, "Anna Mueller") {
		t.Errorf("search filter dropped matching row")
	}
}

func TestUsersJSON_ReturnsValidJSON(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	seedAccessUsers(env,
		access.User{ID: "u1", FirstName: "Sascha", LastName: "Daemgen", Status: access.StatusActive},
	)
	resp, err := env.client.Get(env.ts.URL + "/a/users.json")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	var payload struct {
		Configured bool `json:"configured"`
		Total      int  `json:"total"`
		Users      []struct {
			ID          string `json:"ID"`
			DisplayName string `json:"DisplayName"`
		} `json:"users"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !payload.Configured {
		t.Error("Configured = false, want true")
	}
	if payload.Total != 1 || len(payload.Users) != 1 || payload.Users[0].ID != "u1" {
		t.Errorf("payload wrong: %+v", payload)
	}
}

func TestUsersCreate_StoresAndRedirects(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	form := url.Values{}
	form.Set("first_name", "Otto")
	form.Set("last_name", "Neumann")
	form.Set("email", "otto@n.com")
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/a/users", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	if len(env.userStore.users) != 1 {
		t.Fatalf("userStore has %d users, want 1", len(env.userStore.users))
	}
	got := env.userStore.users[0]
	if got.FirstName != "Otto" || got.LastName != "Neumann" || got.Email != "otto@n.com" {
		t.Errorf("created user wrong: %+v", got)
	}
}

func TestUsersStatus_ActivateDeactivate(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	seedAccessUsers(env,
		access.User{ID: "u1", FirstName: "Sascha", LastName: "Daemgen", Status: access.StatusActive},
	)
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/a/users/u1/deactivate", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	resp.Body.Close()
	if env.userStore.users[0].Status != access.StatusDeactivated {
		t.Errorf("after deactivate status = %q", env.userStore.users[0].Status)
	}
	req2, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/a/users/u1/activate", nil)
	resp2, err := env.client.Do(req2)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	resp2.Body.Close()
	if env.userStore.users[0].Status != access.StatusActive {
		t.Errorf("after activate status = %q", env.userStore.users[0].Status)
	}
}

func TestUsersDetail_RendersLinkedViewers(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	seedAccessUsers(env,
		access.User{ID: "u1", FirstName: "Sascha", LastName: "Daemgen", Status: access.StatusActive},
	)
	// Viewer mit linked_ua_user_id = "u1" seeden
	env.seedViewerAs(t, "0c:ea:14:dd:ee:01", "Wohnung A", "TestPw-1234567X")
	if err := env.viewerMgr.SetLinkedUAUserID(context.Background(), "0c:ea:14:dd:ee:01", "u1"); err != nil {
		t.Fatalf("SetLinkedUAUserID: %v", err)
	}
	resp, err := env.client.Get(env.ts.URL + "/a/users/u1")
	if err != nil {
		t.Fatalf("GET detail: %v", err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, "Wohnung A") {
		t.Errorf("detail page missing linked viewer 'Wohnung A'")
	}
}

func TestViewerCreate_StoresLinkedUserID(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	seedAccessUsers(env,
		access.User{ID: "u1", FirstName: "Sascha", LastName: "Daemgen", Status: access.StatusActive},
	)
	form := url.Values{}
	form.Set("name", "Familie Mueller-Link-Test")
	form.Set("linked_ua_user_id", "u1")
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/a/web-viewers", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// MAC kommt jetzt vom Server (HOTFIX4: kein MAC-Input mehr).
	// Wir suchen den Viewer per Name.
	info, _, err := env.viewerMgr.LookupByName(context.Background(), "Familie Mueller-Link-Test")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if info.LinkedUAUserID != "u1" {
		t.Errorf("LinkedUAUserID = %q, want u1", info.LinkedUAUserID)
	}
}

// TestCreateViewer_RejectsDuplicateName (HOTFIX4): doppelter
// normalisierter Name -> 409 Conflict.
func TestCreateViewer_RejectsDuplicateName(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	form := url.Values{}
	form.Set("name", "Familie Mueller 2OG")
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/a/web-viewers", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, _ := env.client.Do(req)
	resp.Body.Close()

	dupForm := url.Values{}
	dupForm.Set("name", "FAMILIE MUELLER 2OG")
	req2, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/a/web-viewers", strings.NewReader(dupForm.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp2, err := env.client.Do(req2)
	if err != nil {
		t.Fatalf("dup POST: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("dup status = %d, want 409", resp2.StatusCode)
	}
}

// TestSetPassword_UpdatesAndInvalidatesSessions (HOTFIX4): POST
// /a/web-viewers/{mac}/set-password setzt das Passwort und
// loggt offene Sessions aus.
func TestSetPassword_UpdatesAndInvalidatesSessions(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	// Mieter loggt sich ein.
	loginResp := env.loginViewer(t, testViewerLogin, testViewerPassword)
	loginResp.Body.Close()

	var sessionsBefore int
	env.d.QueryRow(`SELECT COUNT(*) FROM viewer_sessions WHERE viewer_mac = ?`, testViewerMAC).Scan(&sessionsBefore)
	if sessionsBefore == 0 {
		t.Fatal("seed login did not create a session")
	}

	// Admin setzt neues Passwort "1234" (Klingel-Anlage, keine
	// Mindest-Laenge).
	body := strings.NewReader(`{"password":"1234"}`)
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/a/web-viewers/"+testViewerMAC+"/set-password", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("set-password: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Session-Tabelle ist leer
	var sessionsAfter int
	env.d.QueryRow(`SELECT COUNT(*) FROM viewer_sessions WHERE viewer_mac = ?`, testViewerMAC).Scan(&sessionsAfter)
	if sessionsAfter != 0 {
		t.Errorf("sessions after set-password = %d, want 0", sessionsAfter)
	}

	// Login mit neuem (kurzem) Passwort funktioniert
	jar2, _ := cookiejar.New(nil)
	fresh := &http.Client{
		Jar: jar2,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	form := url.Values{}
	form.Set("username", testViewerLogin)
	form.Set("password", "1234")
	loginReq, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/login", strings.NewReader(form.Encode()))
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginResp2, err := fresh.Do(loginReq)
	if err != nil {
		t.Fatalf("login with new pw: %v", err)
	}
	loginResp2.Body.Close()
	if loginResp2.StatusCode != http.StatusSeeOther {
		t.Errorf("login with new pw status = %d, want 303", loginResp2.StatusCode)
	}
}

// ---------- Edit + Generate-PW (Saison 13-02-FIX4-a-HOTFIX5) ----------

func TestWebViewerEdit_RenamesAndChangesLink(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	seedAccessUsers(env,
		access.User{ID: "u1", FirstName: "Sascha", LastName: "Daemgen", Status: access.StatusActive},
		access.User{ID: "u2", FirstName: "Anna", LastName: "Schmidt", Status: access.StatusActive},
	)
	env.seedViewerAs(t, "0c:ea:14:ee:ee:01", "Wohnung Alt", "TestPw-1234567X")
	if err := env.viewerMgr.SetLinkedUAUserID(context.Background(), "0c:ea:14:ee:ee:01", "u1"); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	form := url.Values{}
	form.Set("name", "Wohnung Neu")
	form.Set("password", "") // unveraendert
	form.Set("linked_ua_user_id", "u2")
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/a/web-viewers/0c:ea:14:ee:ee:01/edit", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("edit POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["name_changed"] != true {
		t.Errorf("name_changed = %v, want true", got["name_changed"])
	}
	if got["pw_changed"] != false {
		t.Errorf("pw_changed = %v, want false", got["pw_changed"])
	}
	if got["link_changed"] != true {
		t.Errorf("link_changed = %v, want true", got["link_changed"])
	}

	info, err := env.viewerMgr.GetViewerInfo(context.Background(), "0c:ea:14:ee:ee:01")
	if err != nil {
		t.Fatalf("get info: %v", err)
	}
	if info.Name != "Wohnung Neu" {
		t.Errorf("name = %q, want Wohnung Neu", info.Name)
	}
	if info.LinkedUAUserID != "u2" {
		t.Errorf("LinkedUAUserID = %q, want u2", info.LinkedUAUserID)
	}
}

func TestWebViewerEdit_ChangesPasswordAndInvalidatesSessions(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	loginResp := env.loginViewer(t, testViewerLogin, testViewerPassword)
	loginResp.Body.Close()
	var sessionsBefore int
	env.d.QueryRow(`SELECT COUNT(*) FROM viewer_sessions WHERE viewer_mac = ?`, testViewerMAC).Scan(&sessionsBefore)
	if sessionsBefore == 0 {
		t.Fatal("seed login did not create a session")
	}

	form := url.Values{}
	form.Set("name", testViewerName)
	form.Set("password", "neu-1234-Abc")
	form.Set("linked_ua_user_id", "")
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/a/web-viewers/"+testViewerMAC+"/edit", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("edit POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["pw_changed"] != true {
		t.Errorf("pw_changed = %v, want true", got["pw_changed"])
	}
	if got["password"] != "neu-1234-Abc" {
		t.Errorf("password = %v, want neu-1234-Abc", got["password"])
	}

	var sessionsAfter int
	env.d.QueryRow(`SELECT COUNT(*) FROM viewer_sessions WHERE viewer_mac = ?`, testViewerMAC).Scan(&sessionsAfter)
	if sessionsAfter != 0 {
		t.Errorf("sessions after edit pw = %d, want 0", sessionsAfter)
	}
}

func TestWebViewerEdit_RejectsDuplicateName(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewerAs(t, "0c:ea:14:11:11:01", "Wohnung A", "TestPw-1234567X")
	env.seedViewerAs(t, "0c:ea:14:11:11:02", "Wohnung B", "TestPw-1234567X")

	form := url.Values{}
	form.Set("name", "wohnung b") // case-insensitive duplicate
	form.Set("password", "")
	form.Set("linked_ua_user_id", "")
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/a/web-viewers/0c:ea:14:11:11:01/edit", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("edit POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

func TestWebViewerGeneratePW_ReturnsPasswordWithoutSaving(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	// Original-Hash holen; nach Generate-PW darf der sich nicht aendern.
	infoBefore, err := env.viewerMgr.GetViewerInfo(context.Background(), testViewerMAC)
	if err != nil {
		t.Fatalf("get viewer info: %v", err)
	}
	pwSetBefore := infoBefore.PasswordSetAt

	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/a/web-viewers/"+testViewerMAC+"/generate-pw", nil)
	req.Header.Set("Accept", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("generate-pw POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["password"] == "" {
		t.Errorf("password empty")
	}
	if len(got["password"]) < 12 {
		t.Errorf("password too short: %q", got["password"])
	}

	// Hash-Stempel ist unveraendert -> nichts wurde gespeichert.
	infoAfter, err := env.viewerMgr.GetViewerInfo(context.Background(), testViewerMAC)
	if err != nil {
		t.Fatalf("get viewer info after: %v", err)
	}
	if pwSetBefore != nil && infoAfter.PasswordSetAt != nil &&
		!pwSetBefore.Equal(*infoAfter.PasswordSetAt) {
		t.Errorf("generate-pw saved silently (timestamp changed)")
	}

	// Mieter-Login mit dem altem Passwort muss noch funktionieren.
	jar2, _ := cookiejar.New(nil)
	fresh := &http.Client{
		Jar: jar2,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	form := url.Values{}
	form.Set("username", testViewerLogin)
	form.Set("password", testViewerPassword)
	loginReq, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/login", strings.NewReader(form.Encode()))
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginResp, err := fresh.Do(loginReq)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusSeeOther {
		t.Errorf("login with original pw status = %d, want 303 (generate-pw should NOT have changed it)", loginResp.StatusCode)
	}
}

// ---------- Login-Info Endpoint (Saison 13-02-FIX4-a-HOTFIX8) ----------

func TestLoginInfoEndpoint_ReturnsURLAndQR(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	resp, err := env.client.Get(env.ts.URL + "/a/web-viewers/" + testViewerMAC + "/login-info")
	if err != nil {
		t.Fatalf("GET login-info: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["login_url"] == "" {
		t.Error("login_url empty")
	}
	if !strings.Contains(got["login_url"], "/login") {
		t.Errorf("login_url = %q, want suffix /login", got["login_url"])
	}
	if !strings.Contains(got["qr_svg"], "<svg") {
		t.Errorf("qr_svg missing <svg> markup: %q", got["qr_svg"])
	}
}

func TestLoginInfoEndpoint_404OnUnknownMAC(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	resp, err := env.client.Get(env.ts.URL + "/a/web-viewers/0c:ea:14:99:99:99/login-info")
	if err != nil {
		t.Fatalf("GET login-info: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestWebViewersList_IncludesLoginURLPerRow(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	resp, err := env.client.Get(env.ts.URL + "/a/web-viewers")
	if err != nil {
		t.Fatalf("GET list: %v", err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, `data-action="copy-link"`) {
		t.Error("list missing copy-link icon button")
	}
	if !strings.Contains(body, `/login`) {
		t.Error("list missing login URL in data-url")
	}
}

// ---------- Magic-Link routes are gone ----------

func TestMagicLinkRoutes_AreGone(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	for _, path := range []string{
		"/webviewer/login?t=abcdef",
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
