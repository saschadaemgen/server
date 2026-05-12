// Package httpserver hosts the unifix HTTP surface. Two trees:
//
//	/m/   tenant-facing: magic-link login, home stub, logout.
//	/a/   admin-facing:  login (+ first-run setup), dashboard,
//	                     settings, mock-viewer CRUD, UA-user CRUD.
//
// Pure net/http with Go 1.22 ServeMux pattern routing. No router
// or web-framework dependency. TLS is provided by the standard
// library; in DevMode the listener is plain HTTP and the Secure
// cookie flag is disabled.
package httpserver

import (
	"log/slog"
	"net/http"
	"time"

	"unifix.local/server/internal/auth/admin"
	"unifix.local/server/internal/auth/adminsession"
	"unifix.local/server/internal/auth/magiclink"
	"unifix.local/server/internal/auth/session"
	"unifix.local/server/internal/config"
	"unifix.local/server/internal/doorbellhub"
	"unifix.local/server/internal/doorhistory"
	"unifix.local/server/internal/mockmanager"
	"unifix.local/server/internal/platformconfig"
	"unifix.local/server/internal/uaapi"
)

// Deps bundles every dependency the HTTP layer needs. Pass the
// same struct to New regardless of which sub-set of features
// the caller wants enabled; nullable fields like UA degrade
// gracefully.
//
// Saison 12-06 refactor: Sessions now means MIETER sessions
// (bound to mock_mac). Admin sessions live in their own service.
type Deps struct {
	Config         config.Config
	MagicLink      *magiclink.Service
	Sessions       *session.Service
	AdminSessions  *adminsession.Service
	MockManager    *mockmanager.Manager
	Admin          *admin.Service
	PlatformConfig *platformconfig.Service
	// UA is built lazily by main once the operator has saved a
	// base URL and token. Nil means "not configured yet".
	UA *uaapi.Client
	// Hub fans doorbell events from mockmanager out to per-mock
	// SSE subscribers. Nil disables /m/events with 503.
	Hub *doorbellhub.Hub
	// History persists doorbell events for the /m/ list and the
	// /a/ dashboard statistics (Saison 13-01). Nil means the UI
	// shows an empty list and zero counters.
	History doorhistory.Store
	// EventsHeartbeat overrides the SSE keepalive interval.
	// Zero falls back to defaultEventsHeartbeat (30s); tests
	// inject something shorter.
	EventsHeartbeat time.Duration
	Log             *slog.Logger
}

// Server owns the mux and references the auth services.
type Server struct {
	cfg             config.Config
	magic           *magiclink.Service
	sessions        *session.Service
	adminSessions   *adminsession.Service
	mockMgr         *mockmanager.Manager
	admin           *admin.Service
	platformCfg     *platformconfig.Service
	ua              *uaapi.Client
	hub             *doorbellhub.Hub
	history         doorhistory.Store
	eventsHeartbeat time.Duration
	log             *slog.Logger
	mux             *http.ServeMux
	tpl             *adminTemplates
	uaFactory       func() *uaapi.Client // for late-binding after settings save
}

// New constructs the Server with all routes registered.
func New(deps Deps) (*Server, error) {
	tpl, err := newAdminTemplates()
	if err != nil {
		return nil, err
	}
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	srv := &Server{
		cfg:             deps.Config,
		magic:           deps.MagicLink,
		sessions:        deps.Sessions,
		adminSessions:   deps.AdminSessions,
		mockMgr:         deps.MockManager,
		admin:           deps.Admin,
		platformCfg:     deps.PlatformConfig,
		ua:              deps.UA,
		hub:             deps.Hub,
		history:         deps.History,
		eventsHeartbeat: deps.EventsHeartbeat,
		log:             deps.Log.With("component", "httpserver"),
		mux:             http.NewServeMux(),
		tpl:             tpl,
	}
	srv.routes()
	return srv, nil
}

// SetUAClient lets main swap the UA client at runtime after the
// admin has saved fresh credentials via /a/settings. Safe to
// call with nil to drop the configured client.
func (s *Server) SetUAClient(c *uaapi.Client) {
	s.ua = c
}

func (s *Server) routes() {
	// Tenant tree.
	s.mux.HandleFunc("GET /m/login", s.handleLogin)
	s.mux.Handle("POST /m/logout", s.requireSession(http.HandlerFunc(s.handleLogout)))
	s.mux.Handle("GET /m/events", s.requireSession(http.HandlerFunc(s.handleMieterEvents)))
	s.mux.Handle("GET /m/", s.requireSession(http.HandlerFunc(s.handleHome)))

	// Admin tree.
	s.mux.HandleFunc("GET /a/login", s.handleAdminLoginGet)
	s.mux.HandleFunc("POST /a/login", s.handleAdminLoginPost)
	s.mux.Handle("POST /a/logout", s.requireAdminSession(http.HandlerFunc(s.handleAdminLogout)))
	s.mux.Handle("GET /a/{$}", s.requireAdminSession(http.HandlerFunc(s.handleAdminDashboard)))
	s.mux.Handle("GET /a/settings", s.requireAdminSession(http.HandlerFunc(s.handleAdminSettingsGet)))
	s.mux.Handle("POST /a/settings", s.requireAdminSession(http.HandlerFunc(s.handleAdminSettingsPost)))
	s.mux.Handle("GET /a/mocks", s.requireAdminSession(http.HandlerFunc(s.handleAdminMocksList)))
	s.mux.Handle("POST /a/mocks", s.requireAdminSession(http.HandlerFunc(s.handleAdminMocksCreate)))
	s.mux.Handle("DELETE /a/mocks/{mac}", s.requireAdminSession(http.HandlerFunc(s.handleAdminMocksDelete)))
	s.mux.Handle("POST /a/mocks/{mac}/magic-link", s.requireAdminSession(http.HandlerFunc(s.handleAdminMocksMagicLink)))
	s.mux.Handle("GET /a/users", s.requireAdminSession(http.HandlerFunc(s.handleAdminUsersList)))
	s.mux.Handle("POST /a/users", s.requireAdminSession(http.HandlerFunc(s.handleAdminUsersCreate)))
	s.mux.Handle("DELETE /a/users/{id}", s.requireAdminSession(http.HandlerFunc(s.handleAdminUsersDelete)))
}

// Handler returns the underlying mux so callers (tests) can wrap
// it in an httptest.Server.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// ListenAndServe blocks. In DevMode it serves plain HTTP, in TLS
// mode it serves https from CertFile and KeyFile.
func (s *Server) ListenAndServe() error {
	if s.cfg.DevMode {
		return http.ListenAndServe(s.cfg.ListenAddr, s.mux)
	}
	return http.ListenAndServeTLS(s.cfg.ListenAddr, s.cfg.CertFile, s.cfg.KeyFile, s.mux)
}
