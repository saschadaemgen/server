// Package httpserver hosts the carvilon HTTP surface. Three trees:
//
//	/login        tenant login form + POST (Wohnungs-Name +
//	              Passwort + bcrypt; split off from the legacy
//	              /einloggen tree).
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
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	"carvilon.local/server/internal/egresstoken"
	"carvilon.local/server/internal/eventbus"
	"carvilon.local/server/internal/platformconfig"
	"carvilon.local/server/internal/streampublish"
	"carvilon.local/server/internal/streams"
	"carvilon.local/server/internal/turnstore"
	"carvilon.local/server/internal/uaapi"
	"carvilon.local/server/internal/viewermanager"
	"carvilon.local/server/internal/weather"
)

// UserStoreLike is the narrow view of access.UserStore the admin
// UI needs, plus an IsConfigured check for the "not configured
// yet" UI paths.
type UserStoreLike interface {
	access.UserStore
	IsConfigured() bool
}

// ICERequester pulls a fresh set of subscriber ICE servers from the cloud
// over the side-channel (the cloud holds the TURN shared secret; the edge
// never does). Satisfied by *sidechannel.Client and wired in by main after
// the client is built. Nil -> the stream-start bundle reports ICE
// unavailable (503), keeping the LAN path unaffected.
type ICERequester interface {
	RequestICE(ctx context.Context) (streampublish.ICEResult, error)
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
	// UserStore is the UserStore wrapper around the UA client
	// (see access/ua). Nil = UA not configured yet; the admin UI
	// then shows a hint instead of an empty list.
	UserStore UserStoreLike
	// Hub fans doorbell events from viewermanager out to per-mock
	// SSE subscribers. Nil disables /m/events with 503.
	Hub *doorbellhub.Hub
	// History persists doorbell events for the /m/ list and the
	// /a/ dashboard statistics. Nil means the UI shows an empty
	// list and zero counters.
	History doorhistory.Store
	// TURNStore persists the TURN/ICE telemetry shown on /a/turn
	// (Saison 18-10). Nil leaves the page empty ("not active").
	TURNStore *turnstore.Store
	// TURNSnapshots caches the latest cloud-pushed live snapshot for
	// the /a/turn live-stats panel. Nil -> no live stats.
	TURNSnapshots *turnstore.SnapshotHolder
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
	// Streams is the video backend seam. Pass any
	// streams.StreamBackend implementation; nil falls back to
	// streams.Unconfigured() inside New so handler code never
	// has to nil-check, only Configured(). The transitional
	// go2rtc client lives at
	// carvilon.local/server/internal/streams.New; the commercial
	// carvilon-streaming-server plugs in via a build tag.
	Streams streams.StreamBackend
	// Weather is the open-meteo client used by the mieter
	// screensaver. Nil disables /webviewer/weather and
	// /a/weather with 503; the screensaver hides its weather
	// block in that case.
	Weather *weather.Client
	// EgressIssuer mints short-lived WHEP egress tokens for
	// GET /webviewer/egress-token (Saison 18-14). Nil (no egress key
	// configured) -> the endpoint soft-503s.
	EgressIssuer *egresstoken.Issuer
	Log          *slog.Logger
}

// Server owns the mux and references the auth services.
type Server struct {
	cfg             config.Config
	sessions        *session.Service
	adminSessions   *adminsession.Service
	viewerMgr       *viewermanager.Manager
	admin           *admin.Service
	platformCfg     *platformconfig.Service
	audit           *loginaudit.Service
	viewerLimiter   *ratelimit.Limiter
	adminLimiter    *ratelimit.Limiter
	ua              *uaapi.Client
	userStore       UserStoreLike
	hub             *doorbellhub.Hub
	history         doorhistory.Store
	turnStore       *turnstore.Store
	turnSnapshots   *turnstore.SnapshotHolder
	eventsHeartbeat time.Duration
	eventBus        *eventbus.Bus
	calls           *doorbellcalls.Service
	streams         streams.StreamBackend
	// streamStats is a dedicated HTTP client to the stream-server's
	// GET /stream/stats (the per-profile + per-client live data the
	// admin dashboard polls). It targets cfg.StreamBackendURL, which
	// serves /stream/stats in BOTH modes: the external go2rtc/stream
	// server (HTTP mode) and the embedded in-process server on :8555
	// (carvilon_stream mode). nil when no backend URL is configured.
	// Kept separate from `streams` because the in-process build's
	// StreamBackend is a wrapper whose List/Consumers count is the
	// coarse per-camera-hub number, not the per-profile stats.
	streamStats  *streams.Client
	weather      *weather.Client
	egressIssuer *egresstoken.Issuer
	// iceRequester pulls subscriber ICE from the cloud for the stream-start
	// bundle. Set post-construction by main (SetICERequester) once the
	// side-channel client exists; nil when the cloud link is unconfigured.
	iceRequester ICERequester
	log          *slog.Logger
	mux          *http.ServeMux
	tpl          *adminTemplates

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
	// Drop nil-checks at every call site by falling back to the
	// 503-default Unconfigured backend.
	if deps.Streams == nil {
		deps.Streams = streams.Unconfigured()
	}
	// Dedicated /stream/stats client for the admin dashboard. Built
	// only when a backend URL is set; New returns ErrNotConfigured on
	// an empty URL, which we treat as "no live stats" (nil).
	var streamStats *streams.Client
	if c, err := streams.New(deps.Config.StreamBackendURL); err == nil {
		streamStats = c
	}
	srv := &Server{
		cfg:             deps.Config,
		sessions:        deps.Sessions,
		adminSessions:   deps.AdminSessions,
		viewerMgr:       deps.ViewerManager,
		admin:           deps.Admin,
		platformCfg:     deps.PlatformConfig,
		audit:           deps.Audit,
		viewerLimiter:   deps.ViewerLimiter,
		adminLimiter:    deps.AdminLimiter,
		ua:              deps.UA,
		userStore:       deps.UserStore,
		hub:             deps.Hub,
		history:         deps.History,
		turnStore:       deps.TURNStore,
		turnSnapshots:   deps.TURNSnapshots,
		eventsHeartbeat: deps.EventsHeartbeat,
		eventBus:        deps.EventBus,
		calls:           deps.DoorbellCalls,
		streams:         deps.Streams,
		streamStats:     streamStats,
		weather:         deps.Weather,
		egressIssuer:    deps.EgressIssuer,
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

// SetICERequester wires the side-channel client as the ICE source for the
// stream-start bundle. main calls this once after building the client and
// BEFORE ListenAndServe, so the field is published to the serving goroutines
// without locking (happens-before the server starts). Nil-safe: an
// unconfigured cloud link leaves it nil and stream-start 503s.
func (s *Server) SetICERequester(r ICERequester) {
	s.iceRequester = r
}

// ServeRelayed runs a relayed control request through this server's OWN mux
// (Saison 19-27, the generic cloud control relay). It reconstructs an
// *http.Request from the relayed parts, sets the curated headers (Authorization
// + Content-Type) and runs it through s.mux via an httptest recorder - so it
// passes through requireViewerAuth + the UNCHANGED handler exactly as a real
// request would. The edge is the auth authority; the relay never inspects the
// credential. fullPath includes any path params so the mux matches its pattern
// (e.g. {door_id}). Returns the captured status, the curated response header
// (Content-Type) and body, or an error only when the request itself could not
// be BUILT (a mechanism failure; a normal HTTP status, incl. 401, rides the
// status return, not the error).
func (s *Server) ServeRelayed(ctx context.Context, method, fullPath, rawQuery string, header map[string]string, body []byte) (status int, respHeader map[string]string, respBody []byte, err error) {
	target := fullPath
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	// Curated request headers only (the relay carries Authorization +
	// Content-Type; never Host/Cookie/etc).
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	out := map[string]string{}
	if ct := rec.Header().Get("Content-Type"); ct != "" {
		out["Content-Type"] = ct
	}
	return rec.Code, out, rec.Body.Bytes(), nil
}

func (s *Server) routes() {
	// Static assets (CSS, JS, icons). Embedded into the binary
	// via go:embed; served with a long Cache-Control.
	s.mux.Handle("GET /static/", staticHandler())

	// Tenant tree. The old /einloggen entry is split into a
	// login form (/login) and a logged-in viewer area
	// (/webviewer/). Old /einloggen URLs still resolve via the
	// 301 redirect block below.
	s.mux.HandleFunc("GET /login", s.handleLoginGet)
	s.mux.HandleFunc("POST /login", s.handleViewerLoginPost)
	s.mux.HandleFunc("POST /webviewer/logout", s.handleViewerLogout)
	s.mux.Handle("GET /webviewer/events", s.requireViewerAuth(http.HandlerFunc(s.handleMieterEvents)))
	// Saison 19-30: the viewer's assigned doors (1:n) for the unlock
	// buttons. Replaces the bare "standby" assumption.
	s.mux.Handle("GET /webviewer/doors", s.requireViewerAuth(http.HandlerFunc(s.handleMieterDoors)))
	// Doorbell lifecycle.
	s.mux.Handle("POST /webviewer/doors/{door_id}/unlock", s.requireViewerAuth(http.HandlerFunc(s.handleMieterUnlock)))
	s.mux.Handle("POST /webviewer/answer", s.requireViewerAuth(http.HandlerFunc(s.handleMieterAnswer)))
	s.mux.Handle("POST /webviewer/reject", s.requireViewerAuth(http.HandlerFunc(s.handleMieterReject)))
	s.mux.Handle("POST /webviewer/end-call", s.requireViewerAuth(http.HandlerFunc(s.handleMieterEndCall)))
	// Live MJPEG passthrough for the ringing overlay (and
	// optionally the idle stream slot).
	s.mux.Handle("GET /webviewer/stream.mjpeg", s.requireViewerAuth(http.HandlerFunc(s.handleMieterStream)))
	// WebRTC signalling proxy. The browser POSTs an SDP offer; we
	// forward to streams.StreamBackend.WebRTCSignalURL for the
	// viewer's resolved profile and stream the SDP answer back.
	// 503 when no backend is configured.
	s.mux.Handle("POST /webviewer/offer", s.requireViewerAuth(http.HandlerFunc(s.handleMieterOffer)))
	// Saison 18-14: short-lived WHEP egress-token issuance. The browser /
	// app requests a 5-min streamID-bound token to present to the cloud
	// WHEP egress. 503 when no egress key is configured.
	s.mux.Handle("GET /webviewer/egress-token", s.requireViewerAuth(http.HandlerFunc(s.handleMieterEgressToken)))
	// Saison 19: stream-start bundle for a remote (Android) subscriber -
	// public WHEP URL + sid-bound egress token + cloud-minted subscriber
	// ICE servers. Same auth as egress-token; a separate handler because the
	// bundle has a cloud dependency (the ICE pull), so egress-token stays
	// single-purpose. 503 when the cloud link or egress key is unavailable.
	s.mux.Handle("GET /webviewer/stream-start", s.requireViewerAuth(http.HandlerFunc(s.handleMieterStreamStart)))
	// Tenant settings (idle-view-mode) and weather pull for the
	// screensaver.
	s.mux.Handle("GET /webviewer/settings", s.requireViewerAuth(http.HandlerFunc(s.handleMieterSettingsGet)))
	s.mux.Handle("POST /webviewer/settings", s.requireViewerAuth(http.HandlerFunc(s.handleMieterSettingsPost)))
	s.mux.Handle("GET /webviewer/weather", s.requireViewerAuth(http.HandlerFunc(s.handleWeather)))
	// Inline-history mode JSON feed (read-marks rows
	// asynchronously so the browser still sees "NEU" on first
	// open).
	s.mux.Handle("GET /webviewer/history.json", s.requireViewerAuth(http.HandlerFunc(s.handleMieterHistoryJSON)))
	// Mieter soft-delete (single + bulk).
	// DELETE /webviewer/history/{event_id} hides one entry,
	// DELETE /webviewer/history hides every currently-visible
	// row. The admin still sees everything via
	// /a/viewers/{mac}/history.
	s.mux.Handle("DELETE /webviewer/history/{event_id}", s.requireViewerAuth(http.HandlerFunc(s.handleMieterHistoryHideOne)))
	s.mux.Handle("DELETE /webviewer/history", s.requireViewerAuth(http.HandlerFunc(s.handleMieterHistoryHideAll)))
	// Read-only unread-doorbell counter for the screensaver
	// badge. Live updates ride the SSE channel; this endpoint
	// hydrates the initial value and recovers from SSE
	// reconnect.
	s.mux.Handle("GET /webviewer/unread-count", s.requireViewerAuth(http.HandlerFunc(s.handleMieterUnreadCount)))

	// FCM push-token registration for the native apps (Saison 16
	// FCM Etappe). POST registers / refreshes, DELETE clears on
	// app logout. Bearer-gated like the rest of /webviewer/*;
	// the viewer MAC comes from the token context.
	s.mux.Handle("POST /webviewer/fcm-token", s.requireViewerAuth(http.HandlerFunc(s.handleMieterFCMToken)))
	s.mux.Handle("DELETE /webviewer/fcm-token", s.requireViewerAuth(http.HandlerFunc(s.handleMieterFCMTokenDelete)))

	s.mux.Handle("GET /webviewer", s.requireViewerAuth(http.HandlerFunc(s.handleHome)))
	s.mux.Handle("GET /webviewer/", s.requireViewerAuth(http.HandlerFunc(s.handleHome)))

	// Legacy redirects. /m was the original mieter tree;
	// /einloggen was its rename. Both stay as 301 permanent
	// redirects to the /login + /webviewer/* split so QR codes,
	// browser bookmarks and stale tabs keep resolving.
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
	// Admin-side weather preview for the station-coordinates
	// form in /a/settings.
	s.mux.Handle("POST /a/settings/station", s.requireAdminSession(http.HandlerFunc(s.handleAdminStationPost)))
	s.mux.Handle("GET /a/weather", s.requireAdminSession(http.HandlerFunc(s.handleWeather)))

	// Web-viewer CRUD (replaces the legacy /a/mocks tree).
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

	// Stream-profile CRUD against the stream-server's
	// /api/profiles registry. Read for the list view + the
	// viewer-edit modal dropdown (fed by /a/streams.json);
	// write side (S15-25) edits / creates / deletes profiles
	// through Client.Put + Client.Delete. The literal /new
	// route stays in front of the /{name} pattern; Go 1.22
	// ServeMux gives literals precedence over wildcards.
	s.mux.Handle("GET /a/streams", s.requireAdminSession(http.HandlerFunc(s.handleAdminStreamsList)))
	s.mux.Handle("GET /a/streams.json", s.requireAdminSession(http.HandlerFunc(s.handleAdminStreamsListJSON)))
	// Live dashboard poll. The literal stats.json segment takes
	// precedence over GET /a/streams/{name} (edit form) in the Go 1.22
	// mux, so it never collides with a profile named edit route.
	s.mux.Handle("GET /a/streams/stats.json", s.requireAdminSession(http.HandlerFunc(s.handleAdminStreamsStatsJSON)))
	s.mux.Handle("GET /a/streams/new", s.requireAdminSession(http.HandlerFunc(s.handleAdminStreamNew)))
	s.mux.Handle("GET /a/streams/{name}", s.requireAdminSession(http.HandlerFunc(s.handleAdminStreamEdit)))
	s.mux.Handle("POST /a/streams", s.requireAdminSession(http.HandlerFunc(s.handleAdminStreamCreate)))
	s.mux.Handle("POST /a/streams/{name}", s.requireAdminSession(http.HandlerFunc(s.handleAdminStreamSave)))
	s.mux.Handle("POST /a/streams/{name}/delete", s.requireAdminSession(http.HandlerFunc(s.handleAdminStreamDelete)))

	// TURN/STUN/ICE admin menu (Saison 18-10). Read-only: a config +
	// live-stats + history view of the cloud TURN relay, fed by the
	// telemetry the cloud forwards over the side-channel.
	s.mux.Handle("GET /a/turn", s.requireAdminSession(http.HandlerFunc(s.handleAdminTurn)))
	s.mux.Handle("GET /a/turn/stats.json", s.requireAdminSession(http.HandlerFunc(s.handleAdminTurnStatsJSON)))

	// Android-Viewer admin tab (Saison 16 Etappe 1). Bearer-
	// auth happens at the /webviewer/* tree; here we just CRUD
	// the viewers-row + the one-shot token reveal.
	s.mux.Handle("GET /a/android-viewers", s.requireAdminSession(http.HandlerFunc(s.handleAdminAndroidViewersList)))
	s.mux.Handle("GET /a/android-viewers.json", s.requireAdminSession(http.HandlerFunc(s.handleAdminAndroidViewersListJSON)))
	s.mux.Handle("POST /a/android-viewers/adopt", s.requireAdminSession(http.HandlerFunc(s.handleAdminAndroidViewersAdopt)))
	s.mux.Handle("POST /a/android-viewers/{mac}/regenerate-token", s.requireAdminSession(http.HandlerFunc(s.handleAdminAndroidViewersRegenerateToken)))
	s.mux.Handle("POST /a/android-viewers/{mac}/delete", s.requireAdminSession(http.HandlerFunc(s.handleAdminAndroidViewersDelete)))

	// Placeholder pages for upcoming features.
	s.mux.Handle("GET /a/esp-pager", s.requireAdminSession(http.HandlerFunc(s.handleAdminEspPager)))

	// JSON endpoint for the custom dropdown in the viewer modals
	// ("Verknuepfte Klingel"). Returns the UA-API intercoms; the
	// door is auto-resolved at doorbell time via
	// uaapi.LookupDoorForIntercom.
	s.mux.Handle("GET /a/intercoms.json", s.requireAdminSession(http.HandlerFunc(s.handleAdminIntercomsJSON)))
	// Saison 19-30: door list for the per-viewer door-assignment UI
	// (assign the door, not the bell). Source = ListDoors (works),
	// supersedes /a/intercoms.json for the assignment dropdowns.
	s.mux.Handle("GET /a/doors.json", s.requireAdminSession(http.HandlerFunc(s.handleAdminDoorsJSON)))

	// ESP discovery. Public endpoints without an auth header -
	// the bearer token only arrives after a successful admin
	// adoption.
	s.mux.HandleFunc("POST /esp/discover", s.handleESPDiscover)
	s.mux.HandleFunc("GET /esp/discover/status", s.handleESPStatus)

	// ESP runtime. Bearer-token-protected; the token is generated
	// during the adoption flow and picked up by the ESP via
	// /esp/discover/status.
	s.mux.Handle("GET /esp/config", s.requireDeviceBearer(http.HandlerFunc(s.handleESPConfig)))
	s.mux.Handle("GET /esp/events", s.requireDeviceBearer(http.HandlerFunc(s.handleESPEvents)))
	s.mux.Handle("GET /esp/heartbeat", s.requireDeviceBearer(http.HandlerFunc(s.handleESPHeartbeat)))
	s.mux.Handle("POST /esp/answer", s.requireDeviceBearer(http.HandlerFunc(s.handleESPAnswer)))
	s.mux.Handle("POST /esp/reject", s.requireDeviceBearer(http.HandlerFunc(s.handleESPReject)))
	s.mux.Handle("POST /esp/unlock", s.requireDeviceBearer(http.HandlerFunc(s.handleESPUnlock)))
	s.mux.Handle("POST /esp/state", s.requireDeviceBearer(http.HandlerFunc(s.handleESPState)))
	s.mux.Handle("GET /esp/stream.mjpeg", s.requireDeviceBearer(http.HandlerFunc(s.handleESPStream)))
	// ESP settings + weather + unread.
	// POST /esp/settings persists partial updates and broadcasts
	// config.changed; /esp/weather and /esp/unread-count are
	// bearer-gated reuses of the mieter endpoints (same
	// response shape, different auth).
	s.mux.Handle("POST /esp/settings", s.requireDeviceBearer(http.HandlerFunc(s.handleESPSettings)))
	s.mux.Handle("GET /esp/weather", s.requireDeviceBearer(http.HandlerFunc(s.handleESPWeather)))
	s.mux.Handle("GET /esp/unread-count", s.requireDeviceBearer(http.HandlerFunc(s.handleESPUnreadCount)))

	// ESP pendant to /webviewer/history*. Bearer-gated
	// soft-delete + paged list. Internally delegate to the
	// serveHistory* helpers in handler_mieter_history.go.
	s.mux.Handle("GET /esp/history.json", s.requireDeviceBearer(http.HandlerFunc(s.handleESPHistoryList)))
	s.mux.Handle("DELETE /esp/history/{event_id}", s.requireDeviceBearer(http.HandlerFunc(s.handleESPHistoryDeleteOne)))
	s.mux.Handle("DELETE /esp/history", s.requireDeviceBearer(http.HandlerFunc(s.handleESPHistoryDeleteAll)))

	// ESP-viewer admin tab.
	s.mux.Handle("GET /a/esp-viewers", s.requireAdminSession(http.HandlerFunc(s.handleAdminESPViewersList)))
	s.mux.Handle("GET /a/esp-viewers.json", s.requireAdminSession(http.HandlerFunc(s.handleAdminESPViewersListJSON)))
	s.mux.Handle("POST /a/esp-viewers/adopt", s.requireAdminSession(http.HandlerFunc(s.handleAdminESPViewersAdopt)))
	s.mux.Handle("POST /a/esp-viewers/{mac}/reject", s.requireAdminSession(http.HandlerFunc(s.handleAdminESPViewersReject)))
	s.mux.Handle("POST /a/esp-viewers/{mac}/rename", s.requireAdminSession(http.HandlerFunc(s.handleAdminESPViewersRename)))
	s.mux.Handle("POST /a/esp-viewers/{mac}/regenerate-token", s.requireAdminSession(http.HandlerFunc(s.handleAdminESPViewersRegenerateToken)))
	s.mux.Handle("POST /a/esp-viewers/{mac}/delete", s.requireAdminSession(http.HandlerFunc(s.handleAdminESPViewersDelete)))
	s.mux.Handle("DELETE /a/esp-viewers/{mac}", s.requireAdminSession(http.HandlerFunc(s.handleAdminESPViewersDelete)))

	// Unified per-viewer detail page + history endpoints.
	// /a/viewers/{mac} is the HTML drill-down view from the list
	// pages; the three /history endpoints deliver paged JSON +
	// hard-delete.
	s.mux.Handle("GET /a/viewers/{mac}", s.requireAdminSession(http.HandlerFunc(s.handleAdminViewerDetail)))
	s.mux.Handle("GET /a/viewers/{mac}/history", s.requireAdminSession(http.HandlerFunc(s.handleAdminViewerHistoryJSON)))
	s.mux.Handle("DELETE /a/viewers/{mac}/history/{event_id}", s.requireAdminSession(http.HandlerFunc(s.handleAdminViewerHistoryDeleteOne)))
	s.mux.Handle("DELETE /a/viewers/{mac}/history", s.requireAdminSession(http.HandlerFunc(s.handleAdminViewerHistoryDeleteAll)))

	// Admin inline-edit endpoints for the detail page.
	// Stammdaten + settings trigger config.changed; password is
	// web-only, regen-token is esp-only. Both checks live in
	// the handlers.
	s.mux.Handle("POST /a/viewers/{mac}/stammdaten", s.requireAdminSession(http.HandlerFunc(s.handleAdminViewerStammdaten)))
	// Saison 19-30: per-viewer 1:n door assignment (all three types).
	// Replace-all (kept for API; the UI uses the per-door endpoints).
	s.mux.Handle("POST /a/viewers/{mac}/doors", s.requireAdminSession(http.HandlerFunc(s.handleAdminViewerDoors)))
	// Saison 19-32: one-step flow - add/remove a single door, persists
	// immediately (no separate "save" that could wipe on empty).
	s.mux.Handle("POST /a/viewers/{mac}/doors/{door_id}", s.requireAdminSession(http.HandlerFunc(s.handleAdminViewerAddDoor)))
	s.mux.Handle("DELETE /a/viewers/{mac}/doors/{door_id}", s.requireAdminSession(http.HandlerFunc(s.handleAdminViewerRemoveDoor)))
	// Saison 19-32: admin-side door open from the viewer lists (per-row
	// "Tuer oeffnen"). Standby semantics; admin-trusted, no door authz.
	s.mux.Handle("POST /a/viewers/{mac}/unlock", s.requireAdminSession(http.HandlerFunc(s.handleAdminViewerUnlock)))
	s.mux.Handle("POST /a/viewers/{mac}/settings", s.requireAdminSession(http.HandlerFunc(s.handleAdminViewerSettings)))
	s.mux.Handle("POST /a/viewers/{mac}/password", s.requireAdminSession(http.HandlerFunc(s.handleAdminViewerPassword)))
	s.mux.Handle("POST /a/viewers/{mac}/regenerate-token", s.requireAdminSession(http.HandlerFunc(s.handleAdminViewerRegenerateToken)))

	// User CRUD. The UA Access Developer API is the
	// source-of-truth; all access goes through the
	// access.UserStore interface.
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

// redirectLegacyM forwards every request under the legacy /m
// path with a 301 to /login or /webviewer (path suffix included).
// Mapping:
//
//	/m         -> /login          (was previously /einloggen)
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

// redirectLegacyEinloggen maps the legacy /einloggen[/*] path to
// the current /login + /webviewer/* split. 301 because the move
// is permanent - browsers may cache the redirect.
func (s *Server) redirectLegacyEinloggen(w http.ResponseWriter, r *http.Request) {
	target := mapLegacyMieterPath(r.URL.Path, "/einloggen")
	if raw := r.URL.RawQuery; raw != "" {
		target += "?" + raw
	}
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

// mapLegacyMieterPath strips the legacy prefix from path and
// returns the current equivalent. The bare prefix (with or
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
		// the trailing slash on the new path so requireViewerAuth
		// can decide whether to send the user further.
		if tail == "/" {
			return "/webviewer/"
		}
		return "/login"
	default:
		return "/webviewer" + tail
	}
}
