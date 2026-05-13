package httpserver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"unifix.local/mock"
	"unifix.local/server/internal/auth/admin"
	"unifix.local/server/internal/auth/adminsession"
	"unifix.local/server/internal/auth/argon2id"
	"unifix.local/server/internal/auth/loginaudit"
	"unifix.local/server/internal/auth/ratelimit"
	"unifix.local/server/internal/auth/session"
	"unifix.local/server/internal/config"
	"unifix.local/server/internal/db"
	"unifix.local/server/internal/doorbellhub"
	"unifix.local/server/internal/doorhistory"
	"unifix.local/server/internal/mockmanager"
	"unifix.local/server/internal/platformconfig"
	"unifix.local/server/internal/secrets"
)

// Saison 13-02-FIX4-a: Magic-Link ist tot. Alle Login-Tests laufen
// jetzt ueber Username + Passwort POST /m. Der Test-Viewer wird
// vor jedem Login mit seedViewer() angelegt; das setzt sowohl die
// viewers-Zeile als auch das Argon2id-Passwort.
const (
	testViewerMAC      = "0c:ea:14:42:42:42"
	testViewerName     = "Familie Mueller 2OG"
	testViewerUsername = "familie-mueller-2og"
	testViewerPassword = "Kp3-mQ7r9-zX2nWv"
)

// testClock is a shared time source whose value can be moved
// forward to exercise expiry paths in both services.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// testEnv carries everything a test needs in one place.
type testEnv struct {
	srv         *Server
	ts          *httptest.Server
	client      *http.Client
	sess        *session.Service
	adminSess   *adminsession.Service
	adminSvc    *admin.Service
	platformCfg *platformconfig.Service
	mockMgr     *mockmanager.Manager
	hub         *doorbellhub.Hub
	history     *doorhistory.SQLStore
	audit       *loginaudit.Service
	d           *db.DB
	clock       *testClock
}

func newTestServer(t *testing.T) *testEnv {
	return newTestServerWithClock(t, time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC))
}

func newTestServerWithClock(t *testing.T, start time.Time) *testEnv {
	t.Helper()
	clock := &testClock{now: start}
	dbPath := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	sess := session.New(d, session.WithClock(clock.Now))
	adminSess := adminsession.New(d, adminsession.WithClock(clock.Now))
	adminSvc := admin.New(d, admin.WithClock(clock.Now))
	auditSvc := loginaudit.NewWithClock(d, clock.Now)

	secKey := make([]byte, 32)
	for i := range secKey {
		secKey[i] = byte(i)
	}
	secSvc, err := secrets.NewWithKey(secKey)
	if err != nil {
		t.Fatalf("secrets.NewWithKey: %v", err)
	}
	platformCfg := platformconfig.New(d, secSvc, platformconfig.WithClock(clock.Now))

	mockMgr := mockmanager.New(d, quietLogger(), mockmanager.Options{
		StateDirBase: filepath.Join(t.TempDir(), "mocks"),
		ServerIPv4:   "127.0.0.1",
		Factory:      fakeManagerFactory,
	})

	historyStore := doorhistory.NewSQLStore(d.DB)
	hub := doorbellhub.New(mockMgr, historyStore, quietLogger())
	hubCtx, hubCancel := context.WithCancel(context.Background())
	go func() { _ = hub.Run(hubCtx) }()

	cfg := config.Config{
		ListenAddr: ":0",
		DBPath:     dbPath,
		DevMode:    true,
		BaseURL:    "http://127.0.0.1",
		ServerIPv4: "127.0.0.1",
	}
	srv, err := New(Deps{
		Config:          cfg,
		Sessions:        sess,
		AdminSessions:   adminSess,
		MockManager:     mockMgr,
		Admin:           adminSvc,
		PlatformConfig:  platformCfg,
		Audit:           auditSvc,
		ViewerLimiter:   ratelimit.NewWithClock(clock.Now),
		AdminLimiter:    ratelimit.NewWithClock(clock.Now),
		Hub:             hub,
		History:         historyStore,
		EventsHeartbeat: 50 * time.Millisecond,
		Log:             quietLogger(),
	})
	if err != nil {
		t.Fatalf("httpserver.New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	t.Cleanup(func() {
		ts.Close()
		hubCancel()
		shutCtx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = mockMgr.Shutdown(shutCtx)
		_ = d.Close()
	})
	return &testEnv{
		srv: srv, ts: ts, client: client,
		sess: sess, adminSess: adminSess, adminSvc: adminSvc,
		platformCfg: platformCfg, mockMgr: mockMgr,
		hub:     hub,
		history: historyStore,
		audit:   auditSvc,
		d:       d, clock: clock,
	}
}

// seedViewer registriert den Standard-Test-Viewer mit Passwort.
func (e *testEnv) seedViewer(t *testing.T) {
	t.Helper()
	e.seedViewerAs(t, testViewerMAC, testViewerName, testViewerUsername, testViewerPassword)
}

func (e *testEnv) seedViewerAs(t *testing.T, mac, name, username, plainPW string) {
	t.Helper()
	infos, err := e.mockMgr.ListViewers(context.Background())
	if err != nil {
		t.Fatalf("seedViewer: ListViewers: %v", err)
	}
	port := uint16(8100 + len(infos))
	if err := e.mockMgr.AddViewer(context.Background(), mockmanager.ViewerSpec{
		MAC:         mac,
		Name:        name,
		ServicePort: port,
		Type:        mockmanager.TypeWeb,
		Username:    username,
	}); err != nil {
		t.Fatalf("seedViewer: AddViewer: %v", err)
	}
	hash, err := argon2id.HashWithPepper(plainPW, "")
	if err != nil {
		t.Fatalf("seedViewer: hash: %v", err)
	}
	if err := e.mockMgr.SetPasswordHash(context.Background(), mac, hash); err != nil {
		t.Fatalf("seedViewer: set hash: %v", err)
	}
}

// loginViewer fuehrt POST /m mit Form-Body durch und liefert die
// Response-Struct (CheckRedirect verhindert Auto-Follow).
func (e *testEnv) loginViewer(t *testing.T, username, password string) *http.Response {
	t.Helper()
	form := url.Values{}
	form.Set("username", username)
	form.Set("password", password)
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/m", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("POST /m: %v", err)
	}
	return resp
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeManagerFactory builds a do-nothing Viewer that satisfies
// the mockmanager.Viewer interface without binding any sockets.
func fakeManagerFactory(cfg mock.Config, _ *slog.Logger) (mockmanager.Viewer, error) {
	return &noopViewer{
		mac:     cfg.MAC,
		events:  make(chan mock.DoorbellEvent, 1),
		cancels: make(chan mock.DoorbellCancelEvent, 1),
		done:    make(chan struct{}),
	}, nil
}

type noopViewer struct {
	mac     string
	events  chan mock.DoorbellEvent
	cancels chan mock.DoorbellCancelEvent
	done    chan struct{}
	once    sync.Once
}

func (v *noopViewer) Run(ctx context.Context) error {
	<-ctx.Done()
	v.once.Do(func() { close(v.done) })
	return ctx.Err()
}
func (v *noopViewer) Events() <-chan mock.DoorbellEvent        { return v.events }
func (v *noopViewer) Cancels() <-chan mock.DoorbellCancelEvent { return v.cancels }
func (v *noopViewer) MAC() string                              { return v.mac }

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, err := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	return string(buf)
}

func findSessionCookie(resp *http.Response) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookieName {
			return c
		}
	}
	return nil
}

// ---------- Login ----------

func TestLogin_HappyPath(t *testing.T) {
	env := newTestServer(t)
	env.seedViewer(t)
	resp := env.loginViewer(t, testViewerUsername, testViewerPassword)
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusSeeOther)
	}
	if loc := resp.Header.Get("Location"); loc != "/m" {
		t.Errorf("Location = %q, want /m", loc)
	}
	cookie := findSessionCookie(resp)
	if cookie == nil {
		t.Fatal("session cookie missing on login response")
	}
	if cookie.Value == "" {
		t.Error("session cookie value is empty")
	}
	resp.Body.Close()

	resp2, err := env.client.Get(env.ts.URL + "/m/")
	if err != nil {
		t.Fatalf("GET /m/: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("home status = %d, want 200", resp2.StatusCode)
	}
	body := readBody(t, resp2)
	if !strings.Contains(body, testViewerName) {
		t.Errorf("home body missing viewer name %q", testViewerName)
	}
	if !strings.Contains(body, "Bereit") {
		t.Errorf("home body missing intercom-idle hint")
	}
}

func TestLogin_NoSession_RendersForm(t *testing.T) {
	env := newTestServer(t)
	resp, err := env.client.Get(env.ts.URL + "/m")
	if err != nil {
		t.Fatalf("GET /m: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `name="username"`) {
		t.Errorf("login form missing username field")
	}
	if !strings.Contains(body, `name="password"`) {
		t.Errorf("login form missing password field")
	}
}

func TestLogin_PrefillsFromQueryParams(t *testing.T) {
	env := newTestServer(t)
	resp, err := env.client.Get(env.ts.URL + "/m?u=alice&p=hunter2")
	if err != nil {
		t.Fatalf("GET /m: %v", err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, `value="alice"`) {
		t.Errorf("prefill u missing in body")
	}
	if !strings.Contains(body, `value="hunter2"`) {
		t.Errorf("prefill p missing in body")
	}
}

func TestLogin_WrongPassword(t *testing.T) {
	env := newTestServer(t)
	env.seedViewer(t)
	resp := env.loginViewer(t, testViewerUsername, "wrong-password-1234")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (form re-render)", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Falscher") {
		t.Errorf("body missing falsch hint, got: %s", body)
	}
}

func TestLogin_UnknownUser(t *testing.T) {
	env := newTestServer(t)
	resp := env.loginViewer(t, "ghost", "anything-1234")
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, "Falscher") {
		t.Errorf("body missing falsch hint")
	}
}

func TestLogin_RateLimitsAfterRepeatedFailures(t *testing.T) {
	env := newTestServer(t)
	env.seedViewer(t)
	for i := 0; i < ratelimit.IPMaxAttempts; i++ {
		resp := env.loginViewer(t, testViewerUsername, "bad")
		resp.Body.Close()
	}
	resp := env.loginViewer(t, testViewerUsername, testViewerPassword)
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, "Zu viele Versuche") {
		t.Errorf("body missing rate-limit hint, got: %s", body)
	}
}

// ---------- Home ----------

func TestHome_RequiresSession(t *testing.T) {
	env := newTestServer(t)
	resp, err := env.client.Get(env.ts.URL + "/m/")
	if err != nil {
		t.Fatalf("GET /m/: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/m" {
		t.Errorf("Location = %q, want /m", loc)
	}
}

func TestHome_SessionExpired(t *testing.T) {
	env := newTestServer(t)
	env.seedViewer(t)
	resp := env.loginViewer(t, testViewerUsername, testViewerPassword)
	resp.Body.Close()

	env.clock.Add(2 * session.DefaultIdleTimeout)

	resp2, err := env.client.Get(env.ts.URL + "/m/")
	if err != nil {
		t.Fatalf("GET /m/: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303 (expired)", resp2.StatusCode)
	}
}

// ---------- Logout ----------

func TestLogout_RevokesSessionAndClearsCookie(t *testing.T) {
	env := newTestServer(t)
	env.seedViewer(t)
	resp := env.loginViewer(t, testViewerUsername, testViewerPassword)
	resp.Body.Close()

	logout, err := http.NewRequest(http.MethodPost, env.ts.URL+"/m/logout", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp2, err := env.client.Do(logout)
	if err != nil {
		t.Fatalf("POST /m/logout: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", resp2.StatusCode)
	}

	resp3, err := env.client.Get(env.ts.URL + "/m/")
	if err != nil {
		t.Fatalf("GET /m/ after logout: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusSeeOther {
		t.Errorf("home after logout = %d, want 303", resp3.StatusCode)
	}
}

// ---------- Cookie flags ----------

func TestCookie_Flags(t *testing.T) {
	env := newTestServer(t)
	env.seedViewer(t)
	resp := env.loginViewer(t, testViewerUsername, testViewerPassword)
	defer resp.Body.Close()
	cookie := findSessionCookie(resp)
	if cookie == nil {
		t.Fatal("session cookie missing")
	}
	if !cookie.HttpOnly {
		t.Error("HttpOnly = false, want true")
	}
	if cookie.Secure {
		t.Error("Secure = true in DevMode, want false")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite = %v, want Strict", cookie.SameSite)
	}
	if cookie.MaxAge != 365*86400 {
		t.Errorf("MaxAge = %d, want %d", cookie.MaxAge, 365*86400)
	}
}

// ---------- Config / TLS-Pfad ----------

func TestListenAndServe_TLSPathRequiresCerts(t *testing.T) {
	cases := []struct {
		name    string
		cfg     config.Config
		wantErr bool
	}{
		{"tls mode without CertFile rejected",
			config.Config{ListenAddr: ":8443", DBPath: "x.db", DevMode: false},
			true,
		},
		{"tls mode with both certs accepted",
			config.Config{ListenAddr: ":8443", DBPath: "x.db", DevMode: false, CertFile: "c.pem", KeyFile: "k.pem"},
			false,
		},
		{"dev mode without certs accepted",
			config.Config{ListenAddr: ":8080", DBPath: "x.db", DevMode: true},
			false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.cfg.Validate()
			if (err != nil) != c.wantErr {
				t.Errorf("Validate() err = %v, wantErr = %v", err, c.wantErr)
			}
		})
	}
}
