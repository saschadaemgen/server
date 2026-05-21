// Package httpserver hosts the carvilon HTTP surface. Three trees:
//
//	/login        tenant login form + POST (Wohnungs-Name +
//	              Passwort + bcrypt; saison-14-02 split off from
//	              the old /einloggen tree).
//	/webviewer/   tenant home (intercom, stream, settings, SSE)
//	              and logout - everything that requires a session.
//	/a/           admin: login + first-run setup, dashboard,
//	              web-viewer CRUD, settings, esp-viewers, users
//	              (UA-API), esp-pager, streams.
//
// The legacy /m and /einloggen entry points stay registered as
// 301 permanent redirects so old bookmarks and QR codes keep
// resolving (see redirectLegacyM / redirectLegacyEinloggen).
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
	"sync"
	"time"

	"carvilon.local/server/internal/access"
	"carvilon.local/server/internal/auth/admin"
	"carvilon.local/server/internal/auth/adminsession"
	"carvilon.local/server/internal/auth/loginaudit"
	"carvilon.local/server/internal/auth/ratelimit"
	"carvilon.local/server/internal/auth/session"
	"carvilon.local/server/internal/config"
	"carvilon.local/server/internal/doorbellcalls"
	"carvilon.local/server/internal/doorbellhub"
	"carvilon.local/server/internal/doorhistory"
	"carvilon.local/server/internal/eventbus"
	"carvilon.local/server/internal/viewermanager"
	"carvilon.local/server/internal/platformconfig"
	"carvilon.local/server/internal/streams"
	"carvilon.local/server/internal/uaapi"
	"carvilon.local/server/internal/weather"
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
	ViewerManager  *viewermanager.Manager
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
	// Hub fans doorbell events from viewermanager out to per-mock
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
	// DoorbellCalls is the lifecycle service the mieter and ESP
	// answer/reject/end-call endpoints arbitrate against. Nil
	// disables the lifecycle path entirely (calls return 503).
	DoorbellCalls *doorbellcalls.Service
	// Streams is the video backend seam (saison-15-01). Pass any
	// streams.StreamBackend implementation; nil falls back to
	// streams.Unconfigured() inside New so handler code never has
	// to nil-check, only Configured(). The transitional go2rtc
	// client lives at carvilon.local/server/internal/streams.New;
	// the commercial carvilon-streaming-server will plug in via a
	// build tag in a later season.
	Streams streams.StreamBackend
	// Weather is the open-meteo client used by the mieter
	// screensaver (saison-14-01b). Nil disables /webviewer/weather
	// and /a/weather with 503; the screensaver hides its weather
	// block in that case.
	Weather *weather.Client
	Log     *slog.Logger
}

// Server owns the mux and references the auth services.
type Server struct {
	cfg             config.Config
	sessions        *session.Service
	adminSessions   *adminsession.Service
	viewerMgr         *viewermanager.Manager
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
	calls           *doorbellcalls.Service
	streams         streams.StreamBackend
	weather         *weather.Client
	log             *slog.Logger
	mux             *http.ServeMux
	tpl             *adminTemplates

	espStateMu sync.RWMutex
	espState   map[string]ESPState
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
	// Saison 15-01: drop nil-checks at every call site by falling
	// back to the 503-default Unconfigured backend.
	if deps.Streams == nil {
		deps.Streams = streams.Unconfigured()
	}
	srv := &Server{
		cfg:             deps.Config,
		sessions:        deps.Sessions,
		adminSessions:   deps.AdminSessions,
		viewerMgr:         deps.ViewerManager,
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
		calls:           deps.DoorbellCalls,
		streams:         deps.Streams,
		weather:         deps.Weather,
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

	// Tenant tree. Saison 14-02 splits the old /einloggen entry
	// into a login form (/login) and a logged-in viewer area
	// (/webviewer/). Old /einloggen URLs still resolve via the
	// 301 redirect block below.
	s.mux.HandleFunc("GET /login", s.handleLoginGet)
	s.mux.HandleFunc("POST /login", s.handleViewerLoginPost)
	s.mux.HandleFunc("POST /webviewer/logout", s.handleViewerLogout)
	s.mux.Handle("GET /webviewer/events", s.requireSession(http.HandlerFunc(s.handleMieterEvents)))
	// Saison 13-03: Klingel-Lifecycle.
	s.mux.Handle("POST /webviewer/doors/{door_id}/unlock", s.requireSession(http.HandlerFunc(s.handleMieterUnlock)))
	s.mux.Handle("POST /webviewer/answer", s.requireSession(http.HandlerFunc(s.handleMieterAnswer)))
	s.mux.Handle("POST /webviewer/reject", s.requireSession(http.HandlerFunc(s.handleMieterReject)))
	s.mux.Handle("POST /webviewer/end-call", s.requireSession(http.HandlerFunc(s.handleMieterEndCall)))
	// Saison 14-01: live MJPEG passthrough for the ringing overlay
	// (and optionally the idle stream slot).
	s.mux.Handle("GET /webviewer/stream.mjpeg", s.requireSession(http.HandlerFunc(s.handleMieterStream)))
	// Saison 15-01: WebRTC signalling proxy. Browser POSTs SDP
	// offer; we forward to streams.StreamBackend.WebRTCSignalURL
	// for the viewer's resolved profile and stream the SDP answer
	// back. 503 when no backend is configured.
	s.mux.Handle("POST /webviewer/offer", s.requireSession(http.HandlerFunc(s.handleMieterOffer)))
	// Saison 14-01b: tenant settings (idle-view-mode) and weather
	// pull for the screensaver.
	s.mux.Handle("GET /webviewer/settings", s.requireSession(http.HandlerFunc(s.handleMieterSettingsGet)))
	s.mux.Handle("POST /webviewer/settings", s.requireSession(http.HandlerFunc(s.handleMieterSettingsPost)))
	s.mux.Handle("GET /webviewer/weather", s.requireSession(http.HandlerFunc(s.handleWeather)))
	// Saison 14-03: inline-history mode JSON feed (read-marks rows
	// asynchronously so the browser still sees "NEU" on first open).
	s.mux.Handle("GET /webviewer/history.json", s.requireSession(http.HandlerFunc(s.handleMieterHistoryJSON)))
	// Saison 14-04-Phase2: Mieter-Soft-Delete (single + bulk).
	// DELETE /webviewer/history/{event_id} versteckt einen Eintrag,
	// DELETE /webviewer/history versteckt alle aktuell sichtbaren.
	// Admin sieht weiter alles via /a/viewers/{mac}/history.
	s.mux.Handle("DELETE /webviewer/history/{event_id}", s.requireSession(http.HandlerFunc(s.handleMieterHistoryHideOne)))
	s.mux.Handle("DELETE /webviewer/history", s.requireSession(http.HandlerFunc(s.handleMieterHistoryHideAll)))
	// Saison 14-03-FIX03: read-only unread-doorbell counter for the
	// screensaver badge. Live updates ride the SSE channel; this
	// endpoint hydrates the initial value and recovers from SSE
	// reconnect.
	s.mux.Handle("GET /webviewer/unread-count", s.requireSession(http.HandlerFunc(s.handleMieterUnreadCount)))
	s.mux.Handle("GET /webviewer", s.requireSession(http.HandlerFunc(s.handleHome)))
	s.mux.Handle("GET /webviewer/", s.requireSession(http.HandlerFunc(s.handleHome)))

	// Legacy redirects. /m was the original mieter tree (pre-S13-02-
	// FIX4-a); /einloggen was its rename. Both stay as 301 permanent
	// redirects to the saison-14-02 split (/login + /webviewer/*) so
	// QR codes, browser bookmarks and stale tabs keep resolving.
	s.mux.HandleFunc("/m", s.redirectLegacyM)
	s.mux.HandleFunc("/m/", s.redirectLegacyM)
	s.mux.HandleFunc("GET /einloggen", s.redirectLegacyEinloggen)
	s.mux.HandleFunc("GET /einloggen/", s.redirectLegacyEinloggen)

	// Admin tree (/a).
	s.mux.HandleFunc("GET /a/login", s.handleAdminLoginGet)
	s.mux.HandleFunc("POST /a/login", s.handleAdminLoginPost)
	s.mux.Handle("POST /a/logout", s.requireAdminSession(http.HandlerFunc(s.handleAdminLogout)))
	s.mux.Handle("GET /a/{$}", s.requireAdminSession(http.HandlerFunc(s.handleAdminDashboard)))

	s.mux.Handle("GET /a/settings", s.requireAdminSession(http.HandlerFunc(s.handleAdminSettingsGet)))
	s.mux.Handle("POST /a/settings", s.requireAdminSession(http.HandlerFunc(s.handleAdminSettingsPost)))
	s.mux.Handle("POST /a/settings/admin-password", s.requireAdminSession(http.HandlerFunc(s.handleAdminPasswordPost)))
	s.mux.Handle("POST /a/settings/unlock", s.requireAdminSession(http.HandlerFunc(s.handleAdminUnlockLock)))
	// Saison 14-01b: admin-side weather preview for the station-
	// coordinates form in /a/settings.
	s.mux.Handle("POST /a/settings/station", s.requireAdminSession(http.HandlerFunc(s.handleAdminStationPost)))
	s.mux.Handle("GET /a/weather", s.requireAdminSession(http.HandlerFunc(s.handleWeather)))

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

	// Stream-Profile-CRUD (Saison 14-01). Proxyt die go2rtc-REST-
	// API hinter Admin-Session. Profile-Wahl pro Viewer geschieht
	// im Web-/ESP-Viewer-Edit-Modal; die Profile-Definition (FFmpeg-
	// Kette etc.) lebt hier.
	s.mux.Handle("GET /a/streams", s.requireAdminSession(http.HandlerFunc(s.handleAdminStreamsList)))
	s.mux.Handle("GET /a/streams.json", s.requireAdminSession(http.HandlerFunc(s.handleAdminStreamsListJSON)))
	s.mux.Handle("POST /a/streams", s.requireAdminSession(http.HandlerFunc(s.handleAdminStreamsCreate)))
	s.mux.Handle("GET /a/streams/{name}", s.requireAdminSession(http.HandlerFunc(s.handleAdminStreamsEdit)))
	s.mux.Handle("POST /a/streams/{name}", s.requireAdminSession(http.HandlerFunc(s.handleAdminStreamsUpdate)))
	s.mux.Handle("POST /a/streams/{name}/delete", s.requireAdminSession(http.HandlerFunc(s.handleAdminStreamsDelete)))
	s.mux.Handle("DELETE /a/streams/{name}", s.requireAdminSession(http.HandlerFunc(s.handleAdminStreamsDelete)))

	// Platzhalter-Seiten fuer kommende Sub-Saison-Briefings.
	s.mux.Handle("GET /a/esp-pager", s.requireAdminSession(http.HandlerFunc(s.handleAdminEspPager)))

	// Saison 13-07: JSON-Endpoint fuer das Custom-Dropdown in
	// den Viewer-Modalen ("Verknuepfte Klingel"). Liefert die
	// UA-API-Intercoms; die Tuer wird im Klingel-Moment
	// automatisch via uaapi.LookupDoorForIntercom resolved.
	s.mux.Handle("GET /a/intercoms.json", s.requireAdminSession(http.HandlerFunc(s.handleAdminIntercomsJSON)))

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
	s.mux.Handle("GET /esp/heartbeat", s.requireESPBearer(http.HandlerFunc(s.handleESPHeartbeat)))
	s.mux.Handle("POST /esp/answer", s.requireESPBearer(http.HandlerFunc(s.handleESPAnswer)))
	s.mux.Handle("POST /esp/reject", s.requireESPBearer(http.HandlerFunc(s.handleESPReject)))
	s.mux.Handle("POST /esp/unlock", s.requireESPBearer(http.HandlerFunc(s.handleESPUnlock)))
	s.mux.Handle("POST /esp/state", s.requireESPBearer(http.HandlerFunc(s.handleESPState)))
	s.mux.Handle("GET /esp/stream.mjpeg", s.requireESPBearer(http.HandlerFunc(s.handleESPStream)))
	// Saison 14-XX ESP-Settings + Weather + Unread.
	// POST /esp/settings persistiert Partial-Updates und
	// broadcastet config.changed; /esp/weather und
	// /esp/unread-count sind Bearer-gated-Re-Uses der
	// Mieter-Endpoints (gleiche Response-Form, andere Auth).
	s.mux.Handle("POST /esp/settings", s.requireESPBearer(http.HandlerFunc(s.handleESPSettings)))
	s.mux.Handle("GET /esp/weather", s.requireESPBearer(http.HandlerFunc(s.handleESPWeather)))
	s.mux.Handle("GET /esp/unread-count", s.requireESPBearer(http.HandlerFunc(s.handleESPUnreadCount)))

	// Saison 14-04-Phase2-FIX06: ESP-Pendant zu /webviewer/history*.
	// Bearer-gated Soft-Delete + Paged-List. Delegieren intern an
	// die serveHistory*-Helfer aus handler_mieter_history.go.
	s.mux.Handle("GET /esp/history.json", s.requireESPBearer(http.HandlerFunc(s.handleESPHistoryList)))
	s.mux.Handle("DELETE /esp/history/{event_id}", s.requireESPBearer(http.HandlerFunc(s.handleESPHistoryDeleteOne)))
	s.mux.Handle("DELETE /esp/history", s.requireESPBearer(http.HandlerFunc(s.handleESPHistoryDeleteAll)))

	// ESP-Viewer-Admin-Tab.
	s.mux.Handle("GET /a/esp-viewers", s.requireAdminSession(http.HandlerFunc(s.handleAdminESPViewersList)))
	s.mux.Handle("GET /a/esp-viewers.json", s.requireAdminSession(http.HandlerFunc(s.handleAdminESPViewersListJSON)))
	s.mux.Handle("POST /a/esp-viewers/adopt", s.requireAdminSession(http.HandlerFunc(s.handleAdminESPViewersAdopt)))
	s.mux.Handle("POST /a/esp-viewers/{mac}/reject", s.requireAdminSession(http.HandlerFunc(s.handleAdminESPViewersReject)))
	s.mux.Handle("POST /a/esp-viewers/{mac}/rename", s.requireAdminSession(http.HandlerFunc(s.handleAdminESPViewersRename)))
	s.mux.Handle("POST /a/esp-viewers/{mac}/regenerate-token", s.requireAdminSession(http.HandlerFunc(s.handleAdminESPViewersRegenerateToken)))
	s.mux.Handle("POST /a/esp-viewers/{mac}/delete", s.requireAdminSession(http.HandlerFunc(s.handleAdminESPViewersDelete)))
	s.mux.Handle("DELETE /a/esp-viewers/{mac}", s.requireAdminSession(http.HandlerFunc(s.handleAdminESPViewersDelete)))

	// Saison 14-04-Phase2: unified per-viewer detail page +
	// history endpoints. /a/viewers/{mac} ist die HTML-Drill-Down-
	// Sicht aus den Listen-Seiten; die drei /history-Endpoints
	// liefern paged JSON + Hard-Delete.
	s.mux.Handle("GET /a/viewers/{mac}", s.requireAdminSession(http.HandlerFunc(s.handleAdminViewerDetail)))
	s.mux.Handle("GET /a/viewers/{mac}/history", s.requireAdminSession(http.HandlerFunc(s.handleAdminViewerHistoryJSON)))
	s.mux.Handle("DELETE /a/viewers/{mac}/history/{event_id}", s.requireAdminSession(http.HandlerFunc(s.handleAdminViewerHistoryDeleteOne)))
	s.mux.Handle("DELETE /a/viewers/{mac}/history", s.requireAdminSession(http.HandlerFunc(s.handleAdminViewerHistoryDeleteAll)))

	// Saison 14-04-Phase2-FIX02: Admin-Inline-Edit-Endpoints fuer
	// die Detail-Seite. Stammdaten + Settings triggern
	// config.changed; Password ist web-only, Regen-Token ist
	// esp-only. Beide Pruefungen leben in den Handlern.
	s.mux.Handle("POST /a/viewers/{mac}/stammdaten", s.requireAdminSession(http.HandlerFunc(s.handleAdminViewerStammdaten)))
	s.mux.Handle("POST /a/viewers/{mac}/settings", s.requireAdminSession(http.HandlerFunc(s.handleAdminViewerSettings)))
	s.mux.Handle("POST /a/viewers/{mac}/password", s.requireAdminSession(http.HandlerFunc(s.handleAdminViewerPassword)))
	s.mux.Handle("POST /a/viewers/{mac}/regenerate-token", s.requireAdminSession(http.HandlerFunc(s.handleAdminViewerRegenerateToken)))

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
// (vor S13-02-FIX4-a-HOTFIX2) mit 301 nach /login bzw. /webviewer
// weiter, inklusive Path-Suffix. Saison-14-02-Mapping:
//
//	/m         -> /login          (war zuvor /einloggen)
//	/m/        -> /webviewer/
//	/m/events  -> /webviewer/events
//	/m/logout  -> /webviewer/logout
func (s *Server) redirectLegacyM(w http.ResponseWriter, r *http.Request) {
	target := mapLegacyMieterPath(r.URL.Path, "/m")
	if raw := r.URL.RawQuery; raw != "" {
		target += "?" + raw
	}
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

// redirectLegacyEinloggen mappt den Saison-13-/14-01-Pfad
// /einloggen[/*] auf die Saison-14-02-Pfade /login + /webviewer/*.
// 301 weil dauerhaft - Browser duerfen den Redirect cachen.
func (s *Server) redirectLegacyEinloggen(w http.ResponseWriter, r *http.Request) {
	target := mapLegacyMieterPath(r.URL.Path, "/einloggen")
	if raw := r.URL.RawQuery; raw != "" {
		target += "?" + raw
	}
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

// mapLegacyMieterPath strips the legacy prefix from path and
// returns the saison-14-02 equivalent. The bare prefix (with or
// without trailing slash) maps to /login (the form), every other
// suffix maps to /webviewer<suffix>. Shared by the /m and
// /einloggen redirect handlers so the routing table is one
// canonical place.
func mapLegacyMieterPath(path, prefix string) string {
	tail := strings.TrimPrefix(path, prefix)
	switch tail {
	case "", "/":
		// The bare prefix used to render the login form; we route
		// it to /login. The trailing slash variant historically
		// rendered the home page when authenticated, so we keep
		// the trailing slash on the new path so requireSession
		// can decide whether to send the user further.
		if tail == "/" {
			return "/webviewer/"
		}
		return "/login"
	default:
		return "/webviewer" + tail
	}
}
