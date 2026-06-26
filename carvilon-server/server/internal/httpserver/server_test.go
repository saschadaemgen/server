package httpserver

import (
	"context"
	"fmt"
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

	"carvilon.local/mock"
	"carvilon.local/server/internal/access"
	"carvilon.local/server/internal/auth/admin"
	"carvilon.local/server/internal/auth/adminsession"
	"carvilon.local/server/internal/auth/argon2id"
	"carvilon.local/server/internal/auth/loginaudit"
	"carvilon.local/server/internal/auth/ratelimit"
	"carvilon.local/server/internal/auth/session"
	"carvilon.local/server/internal/config"
	"carvilon.local/server/internal/db"
	"carvilon.local/server/internal/doorbellcalls"
	"carvilon.local/server/internal/doorbellhub"
	"carvilon.local/server/internal/doorhistory"
	"carvilon.local/server/internal/eventbus"
	"carvilon.local/server/internal/featuregate"
	"carvilon.local/server/internal/mqttbroker"
	"carvilon.local/server/internal/mqttstore"
	"carvilon.local/server/internal/normalize"
	"carvilon.local/server/internal/platformconfig"
	"carvilon.local/server/internal/secrets"
	"carvilon.local/server/internal/viewermanager"
)

// Login goes via Wohnungs-Name (case + umlaut tolerant).
// testViewerLogin is the name the mieter types into the login
// form; it must match the DB name without normalisation.
const (
	testViewerMAC      = "0c:ea:14:42:42:42"
	testViewerName     = "Familie Mueller 2OG"
	testViewerLogin    = "Familie Mueller 2OG"
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
	viewerMgr     *viewermanager.Manager
	hub         *doorbellhub.Hub
	history     *doorhistory.SQLStore
	audit       *loginaudit.Service
	userStore   *fakeUserStore
	mqttStore   *mqttstore.Store
	mqttBroker  *mqttbroker.Manager
	d           *db.DB
	clock       *testClock
}

// fakeUserStore is a deterministic in-memory access.UserStore for
// the httpserver tests. The real UA backend is replaced with this
// fake so handler tests can seed users without a network call.
type fakeUserStore struct {
	configured bool
	users      []access.User
	createErr  error
}

func newFakeUserStore() *fakeUserStore {
	return &fakeUserStore{configured: true}
}

func (f *fakeUserStore) IsConfigured() bool { return f.configured }

func (f *fakeUserStore) List(ctx context.Context, params access.ListParams) (access.ListResult, error) {
	if !f.configured {
		return access.ListResult{}, access.ErrNotConfigured
	}
	q := strings.ToLower(strings.TrimSpace(params.Query))
	filtered := make([]access.User, 0, len(f.users))
	for _, u := range f.users {
		if params.StatusFilter != "" && u.Status != params.StatusFilter {
			continue
		}
		if q != "" &&
			!strings.Contains(strings.ToLower(u.FirstName), q) &&
			!strings.Contains(strings.ToLower(u.LastName), q) &&
			!strings.Contains(strings.ToLower(u.Email), q) {
			continue
		}
		filtered = append(filtered, u)
	}
	total := len(filtered)
	page := params.Page
	if page < 1 {
		page = 1
	}
	size := params.Size
	if size <= 0 {
		size = 20
	}
	start := (page - 1) * size
	end := start + size
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}
	return access.ListResult{Users: filtered[start:end], Total: total}, nil
}

func (f *fakeUserStore) Get(ctx context.Context, id string) (access.User, error) {
	for _, u := range f.users {
		if u.ID == id {
			return u, nil
		}
	}
	return access.User{}, access.ErrNotFound
}

func (f *fakeUserStore) Create(ctx context.Context, params access.CreateUserParams) (access.User, error) {
	if f.createErr != nil {
		return access.User{}, f.createErr
	}
	u := access.User{
		ID:             fmt.Sprintf("user-%d", len(f.users)+1),
		FirstName:      params.FirstName,
		LastName:       params.LastName,
		Email:          params.Email,
		EmployeeNumber: params.EmployeeNumber,
		Status:         access.StatusActive,
	}
	f.users = append(f.users, u)
	return u, nil
}

func (f *fakeUserStore) Update(ctx context.Context, id string, params access.UpdateUserParams) (access.User, error) {
	for i := range f.users {
		if f.users[i].ID == id {
			f.users[i].FirstName = params.FirstName
			f.users[i].LastName = params.LastName
			f.users[i].Email = params.Email
			f.users[i].EmployeeNumber = params.EmployeeNumber
			return f.users[i], nil
		}
	}
	return access.User{}, access.ErrNotFound
}

func (f *fakeUserStore) Delete(ctx context.Context, id string) error {
	for i := range f.users {
		if f.users[i].ID == id {
			f.users = append(f.users[:i], f.users[i+1:]...)
			return nil
		}
	}
	return access.ErrNotFound
}

func (f *fakeUserStore) SetStatus(ctx context.Context, id string, status access.Status) error {
	for i := range f.users {
		if f.users[i].ID == id {
			f.users[i].Status = status
			return nil
		}
	}
	return access.ErrNotFound
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

	viewerMgr := viewermanager.New(d, quietLogger(), viewermanager.Options{
		StateDirBase: filepath.Join(t.TempDir(), "mocks"),
		ServerIPv4:   "127.0.0.1",
		Factory:      fakeManagerFactory,
	})

	historyStore := doorhistory.NewSQLStore(d.DB)
	callsSvc := doorbellcalls.NewWithClock(d.DB, clock.Now)

	// MQTT broker, wired but DISABLED so no listener binds in tests.
	mqttStore := mqttstore.New(d.DB, func(c context.Context) (string, error) {
		return platformCfg.GetSecret(c, platformconfig.KeyViewerPwPepper)
	})
	mqttBroker := mqttbroker.New(mqttStore, mqttbroker.NewConsole(50), quietLogger(),
		t.TempDir(), mqttbroker.Settings{Enabled: false, TCPPort: 1883, TLSPort: 8883})
	hubBus := eventbus.New()
	hub := doorbellhub.NewWithOptions(viewerMgr, historyStore, quietLogger(), doorbellhub.Options{
		Bus:   hubBus,
		Calls: callsSvc,
	})
	hubCtx, hubCancel := context.WithCancel(context.Background())
	go func() { _ = hub.Run(hubCtx) }()

	userStore := newFakeUserStore()

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
		ViewerManager:     viewerMgr,
		Admin:           adminSvc,
		PlatformConfig:  platformCfg,
		Audit:           auditSvc,
		ViewerLimiter:   ratelimit.NewWithClock(clock.Now),
		AdminLimiter:    ratelimit.NewWithClock(clock.Now),
		Hub:             hub,
		History:         historyStore,
		UserStore:       userStore,
		EventBus:        hubBus,
		DoorbellCalls:   callsSvc,
		EventsHeartbeat: 50 * time.Millisecond,
		Features:        featuregate.NewStore(d.DB),
		MQTT:            mqttBroker,
		MQTTStore:       mqttStore,
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
		_ = viewerMgr.Shutdown(shutCtx)
		_ = d.Close()
	})
	return &testEnv{
		srv: srv, ts: ts, client: client,
		sess: sess, adminSess: adminSess, adminSvc: adminSvc,
		platformCfg: platformCfg, viewerMgr: viewerMgr,
		hub:       hub,
		history:   historyStore,
		audit:      auditSvc,
		userStore:  userStore,
		mqttStore:  mqttStore,
		mqttBroker: mqttBroker,
		d:          d, clock: clock,
	}
}

// seedViewer registriert den Standard-Test-Viewer mit Passwort.
func (e *testEnv) seedViewer(t *testing.T) {
	t.Helper()
	e.seedViewerAs(t, testViewerMAC, testViewerName, testViewerPassword)
}

// seedViewerAs follows the no-username schema: the name IS the
// login key.
func (e *testEnv) seedViewerAs(t *testing.T, mac, name, plainPW string) {
	t.Helper()
	infos, err := e.viewerMgr.ListViewers(context.Background())
	if err != nil {
		t.Fatalf("seedViewer: ListViewers: %v", err)
	}
	port := uint16(8100 + len(infos))
	if err := e.viewerMgr.AddViewer(context.Background(), viewermanager.ViewerSpec{
		MAC:         mac,
		Name:        name,
		ServicePort: port,
		Type:        viewermanager.TypeWeb,
	}); err != nil {
		t.Fatalf("seedViewer: AddViewer: %v", err)
	}
	hash, err := argon2id.HashWithPepper(plainPW, "")
	if err != nil {
		t.Fatalf("seedViewer: hash: %v", err)
	}
	if err := e.viewerMgr.SetPasswordHash(context.Background(), mac, hash); err != nil {
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
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("POST /login: %v", err)
	}
	return resp
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeManagerFactory builds a do-nothing Viewer that satisfies
// the viewermanager.Viewer interface without binding any sockets.
func fakeManagerFactory(cfg mock.Config, _ *slog.Logger) (viewermanager.Viewer, error) {
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

	// rejectMu guards the test-recorded reject calls so the
	// mieter-endpoint tests can assert that /webviewer/reject
	// reached the right viewer with the right intercom MAC.
	rejectMu    sync.Mutex
	rejectCalls []rejectCall
	rejectErr   error
}

type rejectCall struct {
	IntercomMAC string
}

func (v *noopViewer) Run(ctx context.Context) error {
	<-ctx.Done()
	v.once.Do(func() { close(v.done) })
	return ctx.Err()
}
func (v *noopViewer) Events() <-chan mock.DoorbellEvent        { return v.events }
func (v *noopViewer) Cancels() <-chan mock.DoorbellCancelEvent { return v.cancels }
func (v *noopViewer) MAC() string                              { return v.mac }
func (v *noopViewer) RejectDoorbell(intercomMAC string) error {
	v.rejectMu.Lock()
	defer v.rejectMu.Unlock()
	v.rejectCalls = append(v.rejectCalls, rejectCall{IntercomMAC: intercomMAC})
	return v.rejectErr
}

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
	resp := env.loginViewer(t, testViewerLogin, testViewerPassword)
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusSeeOther)
	}
	if loc := resp.Header.Get("Location"); loc != "/webviewer/" {
		t.Errorf("Location = %q, want /webviewer/", loc)
	}
	cookie := findSessionCookie(resp)
	if cookie == nil {
		t.Fatal("session cookie missing on login response")
	}
	if cookie.Value == "" {
		t.Error("session cookie value is empty")
	}
	resp.Body.Close()

	resp2, err := env.client.Get(env.ts.URL + "/webviewer/")
	if err != nil {
		t.Fatalf("GET /webviewer/: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("home status = %d, want 200", resp2.StatusCode)
	}
	body := readBody(t, resp2)
	if !strings.Contains(body, testViewerName) {
		t.Errorf("home body missing viewer name %q", testViewerName)
	}
	// home.html is now the screensaver/livestream idle-container;
	// the old intercom-idle "Bereit" copy is gone.
	if !strings.Contains(body, `id="idle-container"`) {
		t.Errorf("home body missing idle-container scaffold")
	}
}

func TestLogin_NoSession_RendersForm(t *testing.T) {
	env := newTestServer(t)
	resp, err := env.client.Get(env.ts.URL + "/login")
	if err != nil {
		t.Fatalf("GET /login: %v", err)
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

// TestLogin_IgnoresQueryPrefill verifies the security fix: ?u=
// and ?p= are no longer mapped onto the form by the server
// handler (pre-fill is an anti-pattern; passwords have no
// business in URLs).
func TestLogin_IgnoresQueryPrefill(t *testing.T) {
	env := newTestServer(t)
	resp, err := env.client.Get(env.ts.URL + "/login?u=alice&p=hunter2")
	if err != nil {
		t.Fatalf("GET /login: %v", err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if strings.Contains(body, `value="alice"`) {
		t.Errorf("username pre-fill leaked into form (should be empty)")
	}
	if strings.Contains(body, `value="hunter2"`) {
		t.Errorf("password pre-fill leaked into form (should be empty)")
	}
}

func TestLogin_WrongPassword(t *testing.T) {
	env := newTestServer(t)
	env.seedViewer(t)
	resp := env.loginViewer(t, testViewerLogin, "wrong-password-1234")
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

// TestLogin_LockedAccountShowsBanner: after a lockout the mieter
// sees an orange banner pointing at the Hausverwaltung.
func TestLogin_LockedAccountShowsBanner(t *testing.T) {
	env := newTestServer(t)
	env.seedViewer(t)
	for i := 0; i < ratelimit.IPMaxAttempts; i++ {
		resp := env.loginViewer(t, testViewerLogin, "bad")
		resp.Body.Close()
	}
	resp := env.loginViewer(t, testViewerLogin, testViewerPassword)
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, "vorruebergehend gesperrt") {
		t.Errorf("locked banner text missing, got: %s", body)
	}
	if !strings.Contains(body, "Hausverwaltung") {
		t.Errorf("locked banner contact hint missing")
	}
	if !strings.Contains(body, "banner-orange") {
		t.Errorf("locked banner is not orange-styled")
	}
}

// TestLogin_SetsCookieAndRedirects is the regression test for a
// live-observed bug: POST with correct credentials returned
// HTTP 200 + login form instead of 303 + Set-Cookie. This
// explicitly asserts cookie + redirect headers + audit log.
func TestLogin_SetsCookieAndRedirects(t *testing.T) {
	env := newTestServer(t)
	env.seedViewer(t)
	resp := env.loginViewer(t, testViewerLogin, testViewerPassword)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (See Other)", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/webviewer/" {
		t.Errorf("Location = %q, want /webviewer/", loc)
	}
	cookie := findSessionCookie(resp)
	if cookie == nil {
		t.Fatal("Set-Cookie header missing on successful login")
	}
	if cookie.Value == "" {
		t.Error("session cookie value is empty")
	}
	if !cookie.HttpOnly {
		t.Error("session cookie not HttpOnly")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("session cookie SameSite = %v, want Strict", cookie.SameSite)
	}

	rows, err := env.audit.Recent(context.Background(), "viewer", 5)
	if err != nil {
		t.Fatalf("audit.Recent: %v", err)
	}
	var sawSuccess bool
	for _, row := range rows {
		if string(row.Outcome) == "success" && row.Username == testViewerLogin {
			sawSuccess = true
			break
		}
	}
	if !sawSuccess {
		t.Errorf("audit log missing success row for %q", testViewerLogin)
	}
}

// TestLogin_AllUmlautVariants: login goes via Wohnungs-Name with
// a normalised comparison form (case + umlauts + whitespace).
// We seed ONE viewer and try every spelling.
func TestLogin_AllUmlautVariants(t *testing.T) {
	env := newTestServer(t)
	env.seedViewerAs(t,
		"0c:ea:14:dd:dd:dd",
		"Daemgen",
		"TestPw-1234567X",
	)

	for _, typed := range []string{"Daemgen", "daemgen", "DAEMGEN", "Dämgen", "  Daemgen  "} {
		t.Run(typed, func(t *testing.T) {
			env.srv.viewerLimiter.ClearUser(normalize.ViewerName(typed))
			resp := env.loginViewer(t, typed, "TestPw-1234567X")
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusSeeOther {
				t.Errorf("status = %d for typed %q, want 303", resp.StatusCode, typed)
			}
		})
	}
}

// TestLogin_WhitespaceTolerant prueft dass Mehrfach-Spaces im
// Login matchen mit Single-Space im DB-Name.
func TestLogin_WhitespaceTolerant(t *testing.T) {
	env := newTestServer(t)
	env.seedViewerAs(t,
		"0c:ea:14:ww:ww:ww",
		"Familie Mueller 2OG",
		"TestPw-1234567X",
	)
	resp := env.loginViewer(t, "Familie  Mueller   2OG", "TestPw-1234567X")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303 (multi-WS should match single-WS)", resp.StatusCode)
	}
}

// ---------- Home ----------

func TestHome_RequiresSession(t *testing.T) {
	env := newTestServer(t)
	resp, err := env.client.Get(env.ts.URL + "/webviewer/")
	if err != nil {
		t.Fatalf("GET /webviewer/: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want /login", loc)
	}
}

func TestHome_SessionExpired(t *testing.T) {
	env := newTestServer(t)
	env.seedViewer(t)
	resp := env.loginViewer(t, testViewerLogin, testViewerPassword)
	resp.Body.Close()

	env.clock.Add(2 * session.DefaultIdleTimeout)

	resp2, err := env.client.Get(env.ts.URL + "/webviewer/")
	if err != nil {
		t.Fatalf("GET /webviewer/: %v", err)
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
	resp := env.loginViewer(t, testViewerLogin, testViewerPassword)
	resp.Body.Close()

	logout, err := http.NewRequest(http.MethodPost, env.ts.URL+"/webviewer/logout", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp2, err := env.client.Do(logout)
	if err != nil {
		t.Fatalf("POST /webviewer/logout: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", resp2.StatusCode)
	}

	resp3, err := env.client.Get(env.ts.URL + "/webviewer/")
	if err != nil {
		t.Fatalf("GET /webviewer/ after logout: %v", err)
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
	resp := env.loginViewer(t, testViewerLogin, testViewerPassword)
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

// ---------- Legacy redirects ----------

// TestOldMRouteRedirects verifies that every legacy /m path
// (bookmarks, old QR codes, external links) resolves with a 301
// into the current split: the root to /login (the form),
// everything else to /webviewer/<suffix>.
func TestOldMRouteRedirects(t *testing.T) {
	env := newTestServer(t)
	cases := []struct {
		path string
		want string
	}{
		{"/m", "/login"},
		{"/m/", "/webviewer/"},
		{"/m/events", "/webviewer/events"},
		{"/m/logout", "/webviewer/logout"},
		{"/m?u=alice", "/login?u=alice"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			resp, err := env.client.Get(env.ts.URL + c.path)
			if err != nil {
				t.Fatalf("GET %s: %v", c.path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusMovedPermanently {
				t.Errorf("status = %d, want 301 for %s", resp.StatusCode, c.path)
			}
			if loc := resp.Header.Get("Location"); loc != c.want {
				t.Errorf("Location = %q, want %q for %s", loc, c.want, c.path)
			}
		})
	}
}

// TestOldEinloggenRouteRedirects is the pendant to
// TestOldMRouteRedirects: the legacy /einloggen path stays as a
// 301 redirect so existing browser bookmarks and QR codes do
// not break.
func TestOldEinloggenRouteRedirects(t *testing.T) {
	env := newTestServer(t)
	cases := []struct {
		path string
		want string
	}{
		{"/einloggen", "/login"},
		{"/einloggen/", "/webviewer/"},
		{"/einloggen/events", "/webviewer/events"},
		{"/einloggen/stream.mjpeg", "/webviewer/stream.mjpeg"},
		{"/einloggen/weather", "/webviewer/weather"},
		{"/einloggen/settings", "/webviewer/settings"},
		{"/einloggen/logout", "/webviewer/logout"},
		{"/einloggen?u=alice", "/login?u=alice"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			resp, err := env.client.Get(env.ts.URL + c.path)
			if err != nil {
				t.Fatalf("GET %s: %v", c.path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusMovedPermanently {
				t.Errorf("status = %d, want 301 for %s", resp.StatusCode, c.path)
			}
			if loc := resp.Header.Get("Location"); loc != c.want {
				t.Errorf("Location = %q, want %q for %s", loc, c.want, c.path)
			}
		})
	}
}

// ---------- modals carry modal-glass ----------

// TestAllModalsHaveGlassClass renders the admin web-viewers page
// including the create modal and asserts the presence of the
// modal-glass element. Without that class the interactions JS
// closes the modal on every inner click.
func TestAllModalsHaveGlassClass(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	resp, err := env.client.Get(env.ts.URL + "/a/web-viewers")
	if err != nil {
		t.Fatalf("GET /a/web-viewers: %v", err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	// Server-seitiges Anlegen-Modal
	if !strings.Contains(body, `class="modal-glass"`) {
		t.Errorf("create-viewer modal-glass missing in web-viewers HTML")
	}
	// JS-injected Credentials-Modal: das Markup ist im Inline-
	// Skript als String; wir greppen einfach nach dem Bauplan.
	if !strings.Contains(body, `<div class="modal-glass">`) {
		t.Errorf("renderCredentials JS template missing modal-glass")
	}
	if !strings.Contains(body, "modal-header") {
		t.Errorf("modal-header (Library-Konvention) missing")
	}
	if !strings.Contains(body, "modal-footer") {
		t.Errorf("modal-footer (Library-Konvention) missing")
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
