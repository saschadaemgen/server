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
	"strings"
	"time"

	"unifix.local/server/internal/access"
	"unifix.local/server/internal/auth/admin"
	"unifix.local/server/internal/auth/adminsession"
	"unifix.local/server/internal/auth/loginaudit"
	"unifix.local/server/internal/auth/ratelimit"
	"unifix.local/server/internal/auth/session"
	"unifix.local/server/internal/config"
	"unifix.local/server/internal/doorbellhub"
	"unifix.local/server/internal/doorhistory"
	"unifix.local/server/internal/eventbus"
	"unifix.local/server/internal/mockmanager"
	"unifix.local/server/internal/platformconfig"
	"unifix.local/server/internal/uaapi"
)

// UserStoreLike ist die schmale Sicht auf access.UserStore die
// das Admin-UI braucht, plus ein IsConfigured-Check fuer die
// "noch nicht eingerichtet"-UI-Pfade.
type UserStoreLike interface {
	access.UserStore
	IsConfigured() bool
}

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
	// UserStore ist der UserStore-Wrapper um den UA-Client (siehe
	// access/ua). Nil = UA noch nicht konfiguriert; das Admin-UI
	// zeigt dann einen Hinweis statt einer leeren Liste.
	UserStore UserStoreLike
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
	// EventBus is the per-viewer push bus the protected ESP
	// runtime endpoints (SSE) subscribe to. Nil falls back to a
	// fresh bus created in New, so callers that don't need to
	// share the bus across packages can leave this empty.
	EventBus *eventbus.Bus
	Log      *slog.Logger
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
	userStore       UserStoreLike
	hub             *doorbellhub.Hub
	history         doorhistory.Store
	eventsHeartbeat time.Duration
	eventBus        *eventbus.Bus
	log             *slog.Logger
	mux             *http.ServeMux
	tpl             *adminTemplates
}

// EventBus exposes the in-process event bus so callers (main,
// the doorbell wire-up in S13-03) can publish to viewers.
func (s *Server) EventBus() *eventbus.Bus { return s.eventBus }

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
	if deps.EventBus == nil {
		deps.EventBus = eventbus.New()
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
		userStore:       deps.UserStore,
		hub:             deps.Hub,
		history:         deps.History,
		eventsHeartbeat: deps.EventsHeartbeat,
		eventBus:        deps.EventBus,
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

	// Tenant tree (/einloggen). Saison 13-02-FIX4-a-HOTFIX2:
	// die alte /m-Familie wird mieterfreundlich umbenannt. Der
	// alte Pfad antwortet mit 301 (siehe /m-Handler unten).
	s.mux.HandleFunc("GET /einloggen", s.handleViewerRoot)
	s.mux.HandleFunc("POST /einloggen", s.handleViewerLoginPost)
	s.mux.HandleFunc("POST /einloggen/logout", s.handleViewerLogout)
	s.mux.Handle("GET /einloggen/events", s.requireSession(http.HandlerFunc(s.handleMieterEvents)))
	s.mux.Handle("GET /einloggen/", s.requireSession(http.HandlerFunc(s.handleHome)))

	// Old /m-Routen liefern 301 nach /einloggen. Bookmark-/QR-
	// Bestand bleibt damit funktionsfaehig. Path-Suffix wird
	// mitgereicht (z.B. /m/events -> /einloggen/events).
	s.mux.HandleFunc("/m", s.redirectLegacyM)
	s.mux.HandleFunc("/m/", s.redirectLegacyM)

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
	s.mux.Handle("POST /a/web-viewers/{mac}/set-password", s.requireAdminSession(http.HandlerFunc(s.handleAdminWebViewersSetPassword)))
	s.mux.Handle("POST /a/web-viewers/{mac}/edit", s.requireAdminSession(http.HandlerFunc(s.handleAdminWebViewersEdit)))
	s.mux.Handle("POST /a/web-viewers/{mac}/generate-pw", s.requireAdminSession(http.HandlerFunc(s.handleAdminWebViewersGeneratePW)))
	s.mux.Handle("GET /a/web-viewers/{mac}/login-info", s.requireAdminSession(http.HandlerFunc(s.handleAdminWebViewersLoginInfo)))
	s.mux.Handle("POST /a/web-viewers/{mac}/unlock", s.requireAdminSession(http.HandlerFunc(s.handleAdminWebViewersUnlock)))
	s.mux.Handle("POST /a/web-viewers/{mac}/rename", s.requireAdminSession(http.HandlerFunc(s.handleAdminWebViewersRename)))
	s.mux.Handle("POST /a/web-viewers/{mac}/delete", s.requireAdminSession(http.HandlerFunc(s.handleAdminWebViewersDelete)))
	s.mux.Handle("POST /a/web-viewers/{mac}/link", s.requireAdminSession(http.HandlerFunc(s.handleAdminWebViewersSetLink)))
	s.mux.Handle("DELETE /a/web-viewers/{mac}", s.requireAdminSession(http.HandlerFunc(s.handleAdminWebViewersDelete)))

	// Platzhalter-Seiten fuer kommende Sub-Saison-Briefings.
	s.mux.Handle("GET /a/esp-pager", s.requireAdminSession(http.HandlerFunc(s.handleAdminEspPager)))

	// ESP-Discovery (Saison 13-02-FIX4-c). Oeffentliche Endpoints
	// ohne Auth-Header - der Token kommt erst nach erfolgreicher
	// Adoption durch den Admin.
	s.mux.HandleFunc("POST /esp/discover", s.handleESPDiscover)
	s.mux.HandleFunc("GET /esp/discover/status", s.handleESPStatus)

	// ESP-Runtime (Saison 13-02-FIX4-d). Bearer-Token-geschuetzt;
	// Token wurde im Adoption-Flow generiert und vom ESP via
	// /esp/discover/status abgeholt.
	s.mux.Handle("GET /esp/config", s.requireESPBearer(http.HandlerFunc(s.handleESPConfig)))
	s.mux.Handle("GET /esp/events", s.requireESPBearer(http.HandlerFunc(s.handleESPEvents)))

	// ESP-Viewer-Admin-Tab.
	s.mux.Handle("GET /a/esp-viewers", s.requireAdminSession(http.HandlerFunc(s.handleAdminESPViewersList)))
	s.mux.Handle("GET /a/esp-viewers.json", s.requireAdminSession(http.HandlerFunc(s.handleAdminESPViewersListJSON)))
	s.mux.Handle("POST /a/esp-viewers/adopt", s.requireAdminSession(http.HandlerFunc(s.handleAdminESPViewersAdopt)))
	s.mux.Handle("POST /a/esp-viewers/{mac}/reject", s.requireAdminSession(http.HandlerFunc(s.handleAdminESPViewersReject)))
	s.mux.Handle("POST /a/esp-viewers/{mac}/rename", s.requireAdminSession(http.HandlerFunc(s.handleAdminESPViewersRename)))
	s.mux.Handle("POST /a/esp-viewers/{mac}/regenerate-token", s.requireAdminSession(http.HandlerFunc(s.handleAdminESPViewersRegenerateToken)))
	s.mux.Handle("POST /a/esp-viewers/{mac}/delete", s.requireAdminSession(http.HandlerFunc(s.handleAdminESPViewersDelete)))
	s.mux.Handle("DELETE /a/esp-viewers/{mac}", s.requireAdminSession(http.HandlerFunc(s.handleAdminESPViewersDelete)))

	// Benutzer-CRUD (Saison 13-02-FIX4-b). UA-Access-Developer-API
	// ist die Source-of-Truth; alle Zugriffe gehen ueber das
	// access.UserStore-Interface.
	s.mux.Handle("GET /a/users", s.requireAdminSession(http.HandlerFunc(s.handleAdminUsersList)))
	s.mux.Handle("GET /a/users.json", s.requireAdminSession(http.HandlerFunc(s.handleAdminUsersListJSON)))
	s.mux.Handle("POST /a/users", s.requireAdminSession(http.HandlerFunc(s.handleAdminUsersCreate)))
	s.mux.Handle("GET /a/users/{id}", s.requireAdminSession(http.HandlerFunc(s.handleAdminUsersDetail)))
	s.mux.Handle("POST /a/users/{id}/update", s.requireAdminSession(http.HandlerFunc(s.handleAdminUsersUpdate)))
	s.mux.Handle("POST /a/users/{id}/activate", s.requireAdminSession(http.HandlerFunc(s.handleAdminUsersActivate)))
	s.mux.Handle("POST /a/users/{id}/deactivate", s.requireAdminSession(http.HandlerFunc(s.handleAdminUsersDeactivate)))
	s.mux.Handle("POST /a/users/{id}/delete", s.requireAdminSession(http.HandlerFunc(s.handleAdminUsersDelete)))
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

// redirectLegacyM leitet alle Anfragen unter dem alten /m-Pfad
// (vor S13-02-FIX4-a-HOTFIX2) mit 301 nach /einloggen weiter,
// inklusive Path-Suffix. /m -> /einloggen, /m/events ->
// /einloggen/events, /m/logout -> /einloggen/logout.
func (s *Server) redirectLegacyM(w http.ResponseWriter, r *http.Request) {
	tail := strings.TrimPrefix(r.URL.Path, "/m")
	target := "/einloggen" + tail
	if raw := r.URL.RawQuery; raw != "" {
		target += "?" + raw
	}
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}
