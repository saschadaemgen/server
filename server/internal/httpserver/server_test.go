package httpserver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"unifix.local/mock"
	"unifix.local/server/internal/auth/admin"
	"unifix.local/server/internal/auth/magiclink"
	"unifix.local/server/internal/auth/session"
	"unifix.local/server/internal/config"
	"unifix.local/server/internal/db"
	"unifix.local/server/internal/doorbellhub"
	"unifix.local/server/internal/mockmanager"
	"unifix.local/server/internal/platformconfig"
	"unifix.local/server/internal/secrets"
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
	magic       *magiclink.Service
	sess        *session.Service
	adminSvc    *admin.Service
	platformCfg *platformconfig.Service
	mockMgr     *mockmanager.Manager
	hub         *doorbellhub.Hub
	d           *db.DB
	clock       *testClock
}

func newTestServer(t *testing.T) *testEnv {
	return newTestServerWithClock(t, time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC))
}

func newTestServerWithClock(t *testing.T, start time.Time) *testEnv {
	t.Helper()
	clock := &testClock{now: start}
	dbPath := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	magic := magiclink.New(d, magiclink.WithClock(clock.Now))
	sess := session.New(d, session.WithClock(clock.Now))
	adminSvc := admin.New(d, admin.WithClock(clock.Now))

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

	hub := doorbellhub.New(mockMgr, quietLogger())
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
		MagicLink:       magic,
		Sessions:        sess,
		MockManager:     mockMgr,
		Admin:           adminSvc,
		PlatformConfig:  platformCfg,
		Hub:             hub,
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
		magic: magic, sess: sess, adminSvc: adminSvc,
		platformCfg: platformCfg, mockMgr: mockMgr,
		hub: hub,
		d:   d, clock: clock,
	}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeManagerFactory builds a do-nothing Viewer that satisfies
// the mockmanager.Viewer interface without binding any sockets.
// Used by every admin httpserver test so the manager can run
// add/remove/list/binding flows headlessly.
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
	token, err := env.magic.Create(context.Background(), "ua-user-1")
	if err != nil {
		t.Fatalf("magic.Create: %v", err)
	}
	resp, err := env.client.Get(env.ts.URL + "/m/login?t=" + token)
	if err != nil {
		t.Fatalf("GET /m/login: %v", err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusSeeOther)
	}
	if loc := resp.Header.Get("Location"); loc != "/m/" {
		t.Errorf("Location = %q, want %q", loc, "/m/")
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
	if !strings.Contains(body, "Willkommen") {
		t.Errorf("home body missing %q marker, got: %s", "Willkommen", body)
	}
	if !strings.Contains(body, "ua-user-1") {
		t.Errorf("home body missing ua_user_id %q, got: %s", "ua-user-1", body)
	}
}

func TestLogin_TokenMissing(t *testing.T) {
	env := newTestServer(t)
	resp, err := env.client.Get(env.ts.URL + "/m/login")
	if err != nil {
		t.Fatalf("GET /m/login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Magic-Link fehlt") {
		t.Errorf("body missing %q marker, got: %s", "Magic-Link fehlt", body)
	}
}

func TestLogin_TokenInvalid(t *testing.T) {
	env := newTestServer(t)
	resp, err := env.client.Get(env.ts.URL + "/m/login?t=garbage")
	if err != nil {
		t.Fatalf("GET /m/login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Link ungueltig") {
		t.Errorf("body missing %q marker, got: %s", "Link ungueltig", body)
	}
}

func TestLogin_TokenExpired(t *testing.T) {
	env := newTestServer(t)
	token, err := env.magic.Create(context.Background(), "ua-user-1")
	if err != nil {
		t.Fatalf("magic.Create: %v", err)
	}
	env.clock.Add(magiclink.DefaultTTL + time.Second)
	resp, err := env.client.Get(env.ts.URL + "/m/login?t=" + token)
	if err != nil {
		t.Fatalf("GET /m/login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Link abgelaufen") {
		t.Errorf("body missing %q marker, got: %s", "Link abgelaufen", body)
	}
}

func TestLogin_TokenAlreadyConsumed(t *testing.T) {
	env := newTestServer(t)
	token, err := env.magic.Create(context.Background(), "ua-user-1")
	if err != nil {
		t.Fatalf("magic.Create: %v", err)
	}
	resp1, err := env.client.Get(env.ts.URL + "/m/login?t=" + token)
	if err != nil {
		t.Fatalf("first GET /m/login: %v", err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusSeeOther {
		t.Fatalf("first login status = %d, want 303", resp1.StatusCode)
	}

	// Fresh client with empty jar to defeat the "already logged in"
	// short-circuit.
	jar2, _ := cookiejar.New(nil)
	fresh := &http.Client{
		Jar: jar2,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp2, err := fresh.Get(env.ts.URL + "/m/login?t=" + token)
	if err != nil {
		t.Fatalf("second GET /m/login: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("second login status = %d, want 400", resp2.StatusCode)
	}
	body := readBody(t, resp2)
	if !strings.Contains(body, "Link wurde schon benutzt") {
		t.Errorf("body missing %q marker, got: %s", "Link wurde schon benutzt", body)
	}
}

func TestLogin_AlreadyLoggedIn_RedirectsHome(t *testing.T) {
	env := newTestServer(t)
	token, err := env.magic.Create(context.Background(), "ua-user-1")
	if err != nil {
		t.Fatalf("magic.Create: %v", err)
	}
	resp1, err := env.client.Get(env.ts.URL + "/m/login?t=" + token)
	if err != nil {
		t.Fatalf("first login: %v", err)
	}
	resp1.Body.Close()

	// Create a second token but do not consume it.
	token2, err := env.magic.Create(context.Background(), "ua-user-2")
	if err != nil {
		t.Fatalf("second magic.Create: %v", err)
	}
	resp2, err := env.client.Get(env.ts.URL + "/m/login?t=" + token2)
	if err != nil {
		t.Fatalf("second GET /m/login: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", resp2.StatusCode)
	}
	if loc := resp2.Header.Get("Location"); loc != "/m/" {
		t.Errorf("Location = %q, want %q", loc, "/m/")
	}

	// token2 must still be consumable from a fresh client.
	jar3, _ := cookiejar.New(nil)
	fresh := &http.Client{
		Jar: jar3,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp3, err := fresh.Get(env.ts.URL + "/m/login?t=" + token2)
	if err != nil {
		t.Fatalf("third GET /m/login: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusSeeOther {
		t.Errorf("token2 should still be valid, status = %d", resp3.StatusCode)
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
	if loc := resp.Header.Get("Location"); loc != "/m/login" {
		t.Errorf("Location = %q, want %q", loc, "/m/login")
	}
}

func TestHome_SessionExpired(t *testing.T) {
	env := newTestServer(t)
	token, err := env.magic.Create(context.Background(), "ua-user-1")
	if err != nil {
		t.Fatalf("magic.Create: %v", err)
	}
	resp1, err := env.client.Get(env.ts.URL + "/m/login?t=" + token)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	resp1.Body.Close()

	// Push past the session idle timeout. Validate will also push
	// expires_at forward on every hit, so this needs to clear that
	// margin too. We jump >2x the timeout to be unambiguous.
	env.clock.Add(2 * session.DefaultIdleTimeout)

	resp2, err := env.client.Get(env.ts.URL + "/m/")
	if err != nil {
		t.Fatalf("GET /m/: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303 (expired)", resp2.StatusCode)
	}
	if loc := resp2.Header.Get("Location"); loc != "/m/login" {
		t.Errorf("Location = %q, want %q", loc, "/m/login")
	}
}

func TestHome_HappyPath_UpdatesLastSeen(t *testing.T) {
	env := newTestServer(t)
	token, err := env.magic.Create(context.Background(), "ua-user-1")
	if err != nil {
		t.Fatalf("magic.Create: %v", err)
	}
	resp1, err := env.client.Get(env.ts.URL + "/m/login?t=" + token)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	resp1.Body.Close()

	var lastSeenBefore int64
	if err := env.d.QueryRow(
		`SELECT last_seen FROM sessions WHERE ua_user_id = ?`, "ua-user-1",
	).Scan(&lastSeenBefore); err != nil {
		t.Fatalf("query last_seen before: %v", err)
	}

	env.clock.Add(time.Hour)

	resp2, err := env.client.Get(env.ts.URL + "/m/")
	if err != nil {
		t.Fatalf("GET /m/: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp2.StatusCode)
	}
	body := readBody(t, resp2)
	if !strings.Contains(body, "ua-user-1") {
		t.Errorf("body missing ua_user_id, got: %s", body)
	}

	var lastSeenAfter int64
	if err := env.d.QueryRow(
		`SELECT last_seen FROM sessions WHERE ua_user_id = ?`, "ua-user-1",
	).Scan(&lastSeenAfter); err != nil {
		t.Fatalf("query last_seen after: %v", err)
	}
	if lastSeenAfter <= lastSeenBefore {
		t.Errorf("last_seen not advanced: before=%d after=%d", lastSeenBefore, lastSeenAfter)
	}
}

// ---------- Logout ----------

func TestLogout_HappyPath(t *testing.T) {
	env := newTestServer(t)
	token, err := env.magic.Create(context.Background(), "ua-user-1")
	if err != nil {
		t.Fatalf("magic.Create: %v", err)
	}
	resp1, err := env.client.Get(env.ts.URL + "/m/login?t=" + token)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	resp1.Body.Close()

	req, err := http.NewRequest(http.MethodPost, env.ts.URL+"/m/logout", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp2, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /m/logout: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusSeeOther {
		t.Errorf("logout status = %d, want 303", resp2.StatusCode)
	}
	if loc := resp2.Header.Get("Location"); loc != "/m/login" {
		t.Errorf("Location = %q, want %q", loc, "/m/login")
	}
	cookie := findSessionCookie(resp2)
	if cookie == nil {
		t.Fatal("clear-cookie header missing on logout response")
	}
	if cookie.MaxAge >= 0 {
		t.Errorf("cleared cookie MaxAge = %d, want negative", cookie.MaxAge)
	}

	resp3, err := env.client.Get(env.ts.URL + "/m/")
	if err != nil {
		t.Fatalf("GET /m/ after logout: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusSeeOther {
		t.Errorf("home after logout status = %d, want 303", resp3.StatusCode)
	}
	if loc := resp3.Header.Get("Location"); loc != "/m/login" {
		t.Errorf("home redirect = %q, want %q", loc, "/m/login")
	}
}

func TestLogout_IdempotentOnNoSession(t *testing.T) {
	env := newTestServer(t)
	req, err := http.NewRequest(http.MethodPost, env.ts.URL+"/m/logout", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /m/logout: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusInternalServerError {
		t.Errorf("logout without session crashed: %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("logout status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/m/login" {
		t.Errorf("Location = %q, want %q", loc, "/m/login")
	}
}

// ---------- Cookie flags ----------

func TestCookie_Flags(t *testing.T) {
	env := newTestServer(t)
	token, err := env.magic.Create(context.Background(), "ua-user-1")
	if err != nil {
		t.Fatalf("magic.Create: %v", err)
	}
	resp, err := env.client.Get(env.ts.URL + "/m/login?t=" + token)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
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
	if cookie.Path != SessionCookiePath {
		t.Errorf("Path = %q, want %q", cookie.Path, SessionCookiePath)
	}
	if cookie.MaxAge != 30*86400 {
		t.Errorf("MaxAge = %d, want %d", cookie.MaxAge, 30*86400)
	}
}

// ---------- Config / TLS-Pfad ----------

func TestListenAndServe_TLSPathRequiresCerts(t *testing.T) {
	cases := []struct {
		name    string
		cfg     config.Config
		wantErr bool
	}{
		{
			name:    "tls mode without CertFile rejected",
			cfg:     config.Config{ListenAddr: ":8443", DBPath: "x.db", DevMode: false},
			wantErr: true,
		},
		{
			name:    "tls mode with both certs accepted",
			cfg:     config.Config{ListenAddr: ":8443", DBPath: "x.db", DevMode: false, CertFile: "c.pem", KeyFile: "k.pem"},
			wantErr: false,
		},
		{
			name:    "dev mode without certs accepted",
			cfg:     config.Config{ListenAddr: ":8080", DBPath: "x.db", DevMode: true},
			wantErr: false,
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
