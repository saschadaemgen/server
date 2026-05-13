// Package httpserver hosts the unifix HTTP surface. Two trees:
//
//	/m/   tenant-facing: username/password login, home (intercom),
//	                     logout (POST), SSE stream.
//	/a/   admin-facing:  login (+ first-run setup), dashboard,
//	                     web-viewer CRUD (mit Passwort-Reset und
//	                     Rate-Limit-Unlock), settings, plus
//	                     Platzhalter-Seiten fuer esp-viewers,
//	                     users (UA-API), esp-pager.
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
	"unifix.local/server/internal/auth/loginaudit"
	"unifix.local/server/internal/auth/ratelimit"
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
type Deps struct {
	Config         config.Config
	Sessions       *session.Service
	AdminSessions  *adminsession.Service
	MockManager    *mockmanager.Manager
	Admin          *admin.Service
	PlatformConfig *platformconfig.Service
	Audit          *loginaudit.Service
	ViewerLimiter  *ratelimit.Limiter
	AdminLimiter   *ratelimit.Limiter
	// UA is built lazily by main once the operator has saved a
	// base URL and token. Nil means "not configured yet".
	UA *uaapi.Client
	// Hub fans doorbell events from mockmanager out to per-mock
	// SSE subscribers. Nil disables /m/events with 503.
	Hub *doorbellhub.Hub
	// History persists doorbell events for the /m/ list and the
	// /a/ dashboard statistics. Nil means the UI shows an empty
	// list and zero counters.
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
	sessions        *session.Service
	adminSessions   *adminsession.Service
	mockMgr         *mockmanager.Manager
	admin           *admin.Service
	platformCfg     *platformconfig.Service
	audit           *loginaudit.Service
	viewerLimiter   *ratelimit.Limiter
	adminLimiter    *ratelimit.Limiter
	ua              *uaapi.Client
	hub             *doorbellhub.Hub
	history         doorhistory.Store
	eventsHeartbeat time.Duration
	log             *slog.Logger
	mux             *http.ServeMux
	tpl             *adminTemplates
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
	if deps.ViewerLimiter == nil {
		deps.ViewerLimiter = ratelimit.New()
	}
	if deps.AdminLimiter == nil {
		deps.AdminLimiter = ratelimit.New()
	}
	srv := &Server{
		cfg:             deps.Config,
		sessions:        deps.Sessions,
		adminSessions:   deps.AdminSessions,
		mockMgr:         deps.MockManager,
		admin:           deps.Admin,
		platformCfg:     deps.PlatformConfig,
		audit:           deps.Audit,
		viewerLimiter:   deps.ViewerLimiter,
		adminLimiter:    deps.AdminLimiter,
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
	// Static assets (CSS, JS, icons). Embedded into the binary
	// via go:embed; served with a long Cache-Control.
	s.mux.Handle("GET /static/", staticHandler())

	// Tenant tree (/m).
	s.mux.HandleFunc("GET /m", s.handleViewerRoot)
	s.mux.HandleFunc("POST /m", s.handleViewerLoginPost)
	s.mux.HandleFunc("POST /m/logout", s.handleViewerLogout)
	s.mux.Handle("GET /m/events", s.requireSession(http.HandlerFunc(s.handleMieterEvents)))
	s.mux.Handle("GET /m/", s.requireSession(http.HandlerFunc(s.handleHome)))

	// Admin tree (/a).
	s.mux.HandleFunc("GET /a/login", s.handleAdminLoginGet)
	s.mux.HandleFunc("POST /a/login", s.handleAdminLoginPost)
	s.mux.Handle("POST /a/logout", s.requireAdminSession(http.HandlerFunc(s.handleAdminLogout)))
	s.mux.Handle("GET /a/{$}", s.requireAdminSession(http.HandlerFunc(s.handleAdminDashboard)))

	s.mux.Handle("GET /a/settings", s.requireAdminSession(http.HandlerFunc(s.handleAdminSettingsGet)))
	s.mux.Handle("POST /a/settings", s.requireAdminSession(http.HandlerFunc(s.handleAdminSettingsPost)))
	s.mux.Handle("POST /a/settings/admin-password", s.requireAdminSession(http.HandlerFunc(s.handleAdminPasswordPost)))
	s.mux.Handle("POST /a/settings/unlock", s.requireAdminSession(http.HandlerFunc(s.handleAdminUnlockLock)))

	// Web-Viewer-CRUD (ersetzt das alte /a/mocks).
	s.mux.Handle("GET /a/web-viewers", s.requireAdminSession(http.HandlerFunc(s.handleAdminWebViewersList)))
	s.mux.Handle("POST /a/web-viewers", s.requireAdminSession(http.HandlerFunc(s.handleAdminWebViewersCreate)))
	s.mux.Handle("POST /a/web-viewers/{mac}/reset-pw", s.requireAdminSession(http.HandlerFunc(s.handleAdminWebViewersResetPW)))
	s.mux.Handle("POST /a/web-viewers/{mac}/unlock", s.requireAdminSession(http.HandlerFunc(s.handleAdminWebViewersUnlock)))
	s.mux.Handle("POST /a/web-viewers/{mac}/rename", s.requireAdminSession(http.HandlerFunc(s.handleAdminWebViewersRename)))
	s.mux.Handle("POST /a/web-viewers/{mac}/delete", s.requireAdminSession(http.HandlerFunc(s.handleAdminWebViewersDelete)))
	s.mux.Handle("DELETE /a/web-viewers/{mac}", s.requireAdminSession(http.HandlerFunc(s.handleAdminWebViewersDelete)))

	// Platzhalter-Seiten fuer kommende Saison-Briefings (FIX4-b,
	// FIX4-c, spaeter FIX4-d). Sie rendern nur eine kleine
	// "kommt bald"-Karte und melden keinen 404.
	s.mux.Handle("GET /a/esp-viewers", s.requireAdminSession(http.HandlerFunc(s.handleAdminEspViewers)))
	s.mux.Handle("GET /a/users", s.requireAdminSession(http.HandlerFunc(s.handleAdminUsersPlaceholder)))
	s.mux.Handle("GET /a/esp-pager", s.requireAdminSession(http.HandlerFunc(s.handleAdminEspPager)))
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
