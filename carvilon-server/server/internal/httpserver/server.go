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
	"carvilon.local/server/internal/console"
	"carvilon.local/server/internal/consolestore"
	"carvilon.local/server/internal/designerstore"
	"carvilon.local/server/internal/dnssd"
	"carvilon.local/server/internal/doorbellcalls"
	"carvilon.local/server/internal/doorbellhub"
	"carvilon.local/server/internal/doorhistory"
	"carvilon.local/server/internal/egresstoken"
	"carvilon.local/server/internal/eventbus"
	"carvilon.local/server/internal/featuregate"
	"carvilon.local/server/internal/logbuf"
	"carvilon.local/server/internal/mqttbroker"
	"carvilon.local/server/internal/mqttstore"
	"carvilon.local/server/internal/nfc"
	"carvilon.local/server/internal/platformconfig"
	"carvilon.local/server/internal/protectapi"
	"carvilon.local/server/internal/readerstore"
	"carvilon.local/server/internal/shellyapi"
	"carvilon.local/server/internal/shellystore"
	"carvilon.local/server/internal/streampublish"
	"carvilon.local/server/internal/streams"
	"carvilon.local/server/internal/streamstore"
	"carvilon.local/server/internal/telegrambot"
	"carvilon.local/server/internal/telegramstore"
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
	// Protect is the UniFi Protect Integration client (Saison 21 -
	// Protect Etappe 1, read-only cameras + sensors in the Device
	// Center). Built lazily like UA; nil means "not configured yet".
	Protect *protectapi.Client
	// Shelly holds one client per configured Shelly device address
	// (Saison 21 - Shelly Etappe 1, read-only switches in the Device
	// Center). Built lazily like UA; empty means "not configured yet".
	Shelly []*shellyapi.Client
	// ShellyStore is the persistent Shelly device set + ignore list
	// (migration 038, Etappe 2). Nil keeps Shelly on the Etappe-1 read
	// paths with no discovery/removal.
	ShellyStore *shellystore.Store
	// ShellyDiscovery is the dnssd source (a *dnssd.Browser) the mDNS
	// auto-discovery coordinator consumes. Nil disables discovery; main
	// owns its lifecycle (Close on shutdown).
	ShellyDiscovery dnssd.Source
	// UserStore is the UserStore wrapper around the UA client
	// (see access/ua). Nil = UA not configured yet; the admin UI
	// then shows a hint instead of an empty list.
	UserStore UserStoreLike
	// NativeUsers is CARVILONs own user store (access/carvilon,
	// migration 034). Always present (a local SQLite table); it is
	// the canonical user source, independent of UA. Nil only in
	// stripped-down setups, in which case the native section of the
	// Benutzer page degrades to a hint.
	NativeUsers access.NativeUserStore
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
	// StreamSnapshots caches the latest cloud-pushed cloud-viewer snapshot
	// (per-stream WHEP consumer counts) for the admin dashboard. Nil -> no
	// cloud-viewer stats. (S20; injected in step 1, consumed by the
	// dashboard handler in step 2.)
	StreamSnapshots *streamstore.SnapshotHolder
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
	// Features is the Saison-20 feature-gating store (license / templates /
	// per-viewer active overrides). Nil leaves /esp/config and
	// /webviewer/settings.json on their plain Resolve*() values with no
	// additive gating block - fully backwards compatible.
	Features *featuregate.Store
	// MQTT is the embedded broker's lifecycle manager (step 1). Nil
	// leaves the MQTT admin page and live console inert ("disabled").
	MQTT *mqttbroker.Manager
	// MQTTStore is the device-credential + ACL persistence the MQTT
	// admin page reads and writes. Nil disables the credential UI.
	MQTTStore *mqttstore.Store
	// Telegram is the bot's lifecycle manager. Nil leaves the Telegram
	// admin page inert ("disabled") and the palette category absent.
	Telegram *telegrambot.Manager
	// TelegramStore is the chat allowlist + pending-chat persistence
	// the Telegram admin page and the editor's chat picker read.
	TelegramStore *telegramstore.Store
	// DesignerStore persists the logic editor's folder tree and its
	// graphs (migration 032). Nil returns 503 on the designer
	// persistence API; the editor keeps an unsaved in-memory canvas.
	DesignerStore *designerstore.Store
	// ReaderStore is the tag-reader registry (migrations 036/037) the
	// Device Center reads (readers appear as source "RPi"). Nil leaves
	// the page without local reader rows.
	ReaderStore *readerstore.Store
	// NFCMonitor owns the persistent per-reader pollers (the reader is
	// infrastructure). A run binds its engine to it via a RunBinding. Nil
	// on a host without readers; the NFC palette category then stays
	// empty and no graph can bind a reader.
	NFCMonitor *nfc.Monitor
	// LogBuffer is the server-wide recent-log ring the designer's
	// System Log tab streams from (main wires it as a tee around the
	// stdout handler). Nil leaves the tab's SSE endpoint on 503.
	LogBuffer *logbuf.Buffer
	// Console is the terminal dock's session framework (Terminal-Track
	// step 1): it bridges each pane's WebSocket to a local shell PTY or an
	// outbound SSH client. Nil leaves the console WS + caps endpoints on
	// 503 (the dock shows the tabs inert).
	Console *console.Manager
	// ConsoleStore persists the terminal dock's saved connection profiles
	// and TOFU host-key pins (migration 033). Nil disables the profile API
	// and ad-hoc SSH still works (host keys just aren't pinned).
	ConsoleStore *consolestore.Store
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
	protect         *protectapi.Client
	shelly          *shellyFleet
	// shellystore is the persistent Shelly device set + ignore list
	// (migration 038, Etappe 2). Nil keeps Shelly on the Etappe-1 read
	// paths with no discovery/removal.
	shellystore *shellystore.Store
	// shellyDisco is the mDNS auto-discovery coordinator; nil disables
	// discovery (the manual list + read paths still work).
	shellyDisco     *shellyDiscovery
	userStore       UserStoreLike
	nativeUsers     access.NativeUserStore
	hub             *doorbellhub.Hub
	history         doorhistory.Store
	turnStore       *turnstore.Store
	turnSnapshots   *turnstore.SnapshotHolder
	streamSnapshots *streamstore.SnapshotHolder
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
	iceRequester  ICERequester
	features      *featuregate.Store
	mqtt          *mqttbroker.Manager
	mqttStore     *mqttstore.Store
	telegram      *telegrambot.Manager
	telegramStore *telegramstore.Store
	designerStore *designerstore.Store
	readerStore   *readerstore.Store
	nfcMonitor    *nfc.Monitor
	logBuf        *logbuf.Buffer
	console       *console.Manager
	consoleStore  *consolestore.Store
	log           *slog.Logger
	// engineLog scopes the designer-run lifecycle lines to the "engine"
	// subsystem (instead of this package's "httpserver"), so the System
	// Log tab attributes them to the engine.
	engineLog *slog.Logger
	mux       *http.ServeMux
	tpl       *adminTemplates

	// designerRuns holds the live logic-editor engine runs, one per
	// admin user (Run executes the posted graph on a wall-clock ticker;
	// the editor streams it back via the monitor SSE).
	designerRuns *designerRunSet

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
		protect:         deps.Protect,
		userStore:       deps.UserStore,
		nativeUsers:     deps.NativeUsers,
		hub:             deps.Hub,
		history:         deps.History,
		turnStore:       deps.TURNStore,
		turnSnapshots:   deps.TURNSnapshots,
		streamSnapshots: deps.StreamSnapshots,
		eventsHeartbeat: deps.EventsHeartbeat,
		eventBus:        deps.EventBus,
		calls:           deps.DoorbellCalls,
		streams:         deps.Streams,
		streamStats:     streamStats,
		weather:         deps.Weather,
		egressIssuer:    deps.EgressIssuer,
		features:        deps.Features,
		mqtt:            deps.MQTT,
		mqttStore:       deps.MQTTStore,
		telegram:        deps.Telegram,
		telegramStore:   deps.TelegramStore,
		designerStore:   deps.DesignerStore,
		readerStore:     deps.ReaderStore,
		nfcMonitor:      deps.NFCMonitor,
		logBuf:          deps.LogBuffer,
		console:         deps.Console,
		consoleStore:    deps.ConsoleStore,
		log:             deps.Log.With("component", "httpserver"),
		engineLog:       deps.Log.With("component", "engine"),
		mux:             http.NewServeMux(),
		tpl:             tpl,
	}
	srv.designerRuns = newDesignerRunSet()
	srv.shellystore = deps.ShellyStore
	srv.SetShellyClients(deps.Shelly)
	// Shelly Etappe 2: wire the mDNS auto-discovery coordinator when a store
	// and a dnssd source are present. main starts it (RunShellyDiscovery) and
	// owns the source's lifecycle.
	if deps.ShellyStore != nil && deps.ShellyDiscovery != nil {
		srv.shellyDisco = newShellyDiscovery(deps.ShellyStore, deps.ShellyDiscovery,
			deps.Log, srv.shellyEnabled, srv.rebuildShellyClients)
	}
	srv.routes()
	return srv, nil
}

// RunShellyDiscovery runs the mDNS auto-discovery coordinator until ctx is
// cancelled. main launches it in its own goroutine; a no-op when discovery
// is not wired.
func (s *Server) RunShellyDiscovery(ctx context.Context) {
	if s.shellyDisco == nil {
		return
	}
	s.shellyDisco.Run(ctx)
}

// SetUAClient lets main swap the UA client at runtime after the
// admin has saved fresh credentials via /a/settings. Safe to
// call with nil to drop the configured client.
func (s *Server) SetUAClient(c *uaapi.Client) {
	s.ua = c
}

// SetProtectClient lets main (and the settings POST) swap the
// Protect client at runtime after the admin has saved fresh
// credentials. Safe to call with nil to drop the configured client.
func (s *Server) SetProtectClient(c *protectapi.Client) {
	s.protect = c
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
	// Saison 19-37: JSON mirror for the app's config.changed refetch
	// (Bearer/cookie auth; relay-allowlisted on :8447 for remote refetch).
	s.mux.Handle("GET /webviewer/settings.json", s.requireViewerAuth(http.HandlerFunc(s.handleMieterSettingsJSON)))
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
	// Protect Etappe 1: its own settings form (host + X-API-KEY + toggle).
	s.mux.Handle("POST /a/settings/protect", s.requireAdminSession(http.HandlerFunc(s.handleAdminProtectSettingsPost)))
	// Shelly Etappe 1: its own settings form (addresses + auth password
	// + toggle) plus the async "Connection" probe for the block's
	// status line (counts only - never addresses).
	s.mux.Handle("POST /a/settings/shelly", s.requireAdminSession(http.HandlerFunc(s.handleAdminShellySettingsPost)))
	s.mux.Handle("GET /a/settings/shelly/status", s.requireAdminSession(http.HandlerFunc(s.handleAdminShellyStatus)))
	// Shelly Etappe 2: active mDNS "Scan now" + releasing a device from the
	// sticky ignore list (both from the settings block).
	s.mux.Handle("POST /a/settings/shelly/scan", s.requireAdminSession(http.HandlerFunc(s.handleAdminShellyScan)))
	s.mux.Handle("POST /a/settings/shelly/release", s.requireAdminSession(http.HandlerFunc(s.handleAdminShellyRelease)))
	// Saison 20: admin UI accent color (single platform_config value).
	s.mux.Handle("POST /a/settings/accent", s.requireAdminSession(http.HandlerFunc(s.handleAdminAccentPost)))
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

	// Logic editor (visual designer). The host page renders the admin
	// chrome with a full-bleed iframe; the iframe loads the
	// self-contained editor bundle served verbatim from the embedded FS
	// under /a/designer/ (exact /a/designer is the host page, the
	// /a/designer/ subtree is the bundle). The palette is fed from the
	// Go block catalog at /a/designer/catalog.json. All admin-gated. Demo
	// data only; live engine/SSE feeds are a later ticket.
	// Saison 21 - UA read-only device + door overview (Etappe 1). The
	// two detail routes are lazily fetched when a row is expanded; the
	// status route is the live poll that keeps the page fresh.
	// CARVILON's own tag readers (registry, migrations 036/037) appear
	// on the same page as source "RPi"; the rename route is the one
	// write the page carries - it only touches OUR reader registry,
	// never UA.
	s.mux.Handle("GET /a/ua", s.requireAdminSession(http.HandlerFunc(s.handleAdminUA)))
	s.mux.Handle("GET /a/ua/status", s.requireAdminSession(http.HandlerFunc(s.handleAdminUAStatus)))
	s.mux.Handle("POST /a/ua/readers/name", s.requireAdminSession(http.HandlerFunc(s.handleAdminUAReaderRename)))
	s.mux.Handle("GET /a/ua/devices/{id}/settings", s.requireAdminSession(http.HandlerFunc(s.handleAdminUADeviceSettings)))
	s.mux.Handle("GET /a/ua/doors/{id}", s.requireAdminSession(http.HandlerFunc(s.handleAdminUADoorDetail)))
	// Protect Etappe 1: lazy camera/sensor detail for the same page.
	s.mux.Handle("GET /a/ua/protect/cameras/{id}", s.requireAdminSession(http.HandlerFunc(s.handleAdminUAProtectCamera)))
	s.mux.Handle("GET /a/ua/protect/sensors/{id}", s.requireAdminSession(http.HandlerFunc(s.handleAdminUAProtectSensor)))
	// Shelly Etappe 1: lazy live channel detail for the same page. The
	// {id} is the configured device address; the handler only ever
	// dials addresses that are part of the stored configuration.
	s.mux.Handle("GET /a/ua/shelly/{id}", s.requireAdminSession(http.HandlerFunc(s.handleAdminUAShellyDetail)))
	// Shelly Etappe 2: sticky per-device removal + active "Scan now" from the
	// Device Center. Removal forgets the device on OUR side (ignore list) and
	// never writes to the device - control stays read-only.
	s.mux.Handle("POST /a/ua/shelly/remove", s.requireAdminSession(http.HandlerFunc(s.handleAdminUAShellyRemove)))
	s.mux.Handle("POST /a/ua/shelly/scan", s.requireAdminSession(http.HandlerFunc(s.handleAdminUAShellyScan)))
	// Dev-only: feed a synthetic mDNS announcement through the real discovery
	// path so the adopt/sticky-remove/release chain can be driven without a
	// live device or OS multicast. Registered ONLY in DevMode - never in a
	// production build.
	if s.cfg.DevMode {
		s.mux.Handle("POST /a/ua/shelly/_dev/announce", s.requireAdminSession(http.HandlerFunc(s.handleAdminShellyDevAnnounce)))
	}

	s.mux.Handle("GET /a/designer", s.requireAdminSession(http.HandlerFunc(s.handleAdminDesigner)))
	s.mux.Handle("GET /a/designer/catalog.json", s.requireAdminSession(http.HandlerFunc(s.handleDesignerCatalog)))
	s.mux.Handle("GET /a/designer/gpio/lines", s.requireAdminSession(http.HandlerFunc(s.handleDesignerGPIOLines)))
	s.mux.Handle("GET /a/designer/telegram/chats", s.requireAdminSession(http.HandlerFunc(s.handleDesignerTelegramChats)))
	s.mux.Handle("GET /a/designer/host", s.requireAdminSession(http.HandlerFunc(s.handleDesignerHost)))
	s.mux.Handle("GET /a/designer/syslog", s.requireAdminSession(http.HandlerFunc(s.handleDesignerSysLog)))
	// Run: execute the posted graph in the engine, stream live values back
	// over the monitor SSE, inject the editor's button press, and tear the
	// run down on stop/disconnect. One run per admin session.
	s.mux.Handle("POST /a/designer/run", s.requireAdminSession(http.HandlerFunc(s.handleDesignerRun)))
	s.mux.Handle("GET /a/designer/run/monitor", s.requireAdminSession(http.HandlerFunc(s.handleDesignerRunMonitor)))
	s.mux.Handle("GET /a/designer/run/status", s.requireAdminSession(http.HandlerFunc(s.handleDesignerRunStatus)))
	s.mux.Handle("POST /a/designer/run/input", s.requireAdminSession(http.HandlerFunc(s.handleDesignerRunInput)))
	s.mux.Handle("POST /a/designer/run/stop", s.requireAdminSession(http.HandlerFunc(s.handleDesignerRunStop)))
	// Persistence (migration 032): the editor's real folder tree +
	// graph CRUD + the ~1s debounced autosave. System folders reply
	// 4xx on every structural mutation.
	s.mux.Handle("GET /a/designer/tree", s.requireAdminSession(http.HandlerFunc(s.handleDesignerTree)))
	s.mux.Handle("POST /a/designer/folders", s.requireAdminSession(http.HandlerFunc(s.handleDesignerFolderCreate)))
	s.mux.Handle("POST /a/designer/folders/{id}/rename", s.requireAdminSession(http.HandlerFunc(s.handleDesignerFolderRename)))
	s.mux.Handle("POST /a/designer/folders/{id}/delete", s.requireAdminSession(http.HandlerFunc(s.handleDesignerFolderDelete)))
	s.mux.Handle("POST /a/designer/graphs", s.requireAdminSession(http.HandlerFunc(s.handleDesignerGraphCreate)))
	s.mux.Handle("GET /a/designer/graphs/{id}", s.requireAdminSession(http.HandlerFunc(s.handleDesignerGraphGet)))
	s.mux.Handle("POST /a/designer/graphs/{id}/rename", s.requireAdminSession(http.HandlerFunc(s.handleDesignerGraphRename)))
	s.mux.Handle("POST /a/designer/graphs/{id}/delete", s.requireAdminSession(http.HandlerFunc(s.handleDesignerGraphDelete)))
	s.mux.Handle("POST /a/designer/graphs/{id}/save", s.requireAdminSession(http.HandlerFunc(s.handleDesignerGraphSave)))
	// Terminal dock (Terminal-Track step 1): the console session frame.
	// The WS endpoint bridges each pane to a local shell PTY or an
	// outbound SSH client; the rest is saved-profile CRUD + TOFU host-key
	// re-trust. All admin-gated; the more specific patterns win over the
	// /a/designer/ static handler below.
	s.mux.Handle("GET /a/designer/console/caps", s.requireAdminSession(http.HandlerFunc(s.handleConsoleCaps)))
	s.mux.Handle("GET /a/designer/console/ws", s.requireAdminSession(http.HandlerFunc(s.handleConsoleWS)))
	s.mux.Handle("GET /a/designer/console/profiles", s.requireAdminSession(http.HandlerFunc(s.handleConsoleProfilesList)))
	s.mux.Handle("POST /a/designer/console/profiles", s.requireAdminSession(http.HandlerFunc(s.handleConsoleProfileCreate)))
	s.mux.Handle("POST /a/designer/console/profiles/{id}", s.requireAdminSession(http.HandlerFunc(s.handleConsoleProfileUpdate)))
	s.mux.Handle("POST /a/designer/console/profiles/{id}/delete", s.requireAdminSession(http.HandlerFunc(s.handleConsoleProfileDelete)))
	s.mux.Handle("POST /a/designer/console/hostkey/forget", s.requireAdminSession(http.HandlerFunc(s.handleConsoleHostKeyForget)))

	s.mux.Handle("GET /a/designer/", s.requireAdminSession(designerStaticHandler()))

	// MQTT broker admin (step 1): device credentials + ACL rules +
	// broker on/off/ports/TLS, plus the live console SSE feed.
	s.mux.Handle("GET /a/mqtt", s.requireAdminSession(http.HandlerFunc(s.handleAdminMQTTGet)))
	s.mux.Handle("POST /a/mqtt/broker", s.requireAdminSession(http.HandlerFunc(s.handleAdminMQTTBrokerPost)))
	s.mux.Handle("POST /a/mqtt/devices", s.requireAdminSession(http.HandlerFunc(s.handleAdminMQTTDeviceCreate)))
	s.mux.Handle("POST /a/mqtt/devices/{username}/set-password", s.requireAdminSession(http.HandlerFunc(s.handleAdminMQTTDeviceSetPassword)))
	s.mux.Handle("POST /a/mqtt/devices/{username}/delete", s.requireAdminSession(http.HandlerFunc(s.handleAdminMQTTDeviceDelete)))
	s.mux.Handle("POST /a/mqtt/acl", s.requireAdminSession(http.HandlerFunc(s.handleAdminMQTTACLAdd)))
	s.mux.Handle("POST /a/mqtt/acl/{id}/delete", s.requireAdminSession(http.HandlerFunc(s.handleAdminMQTTACLDelete)))
	s.mux.Handle("GET /a/mqtt/monitor", s.requireAdminSession(http.HandlerFunc(s.handleAdminMQTTMonitor)))
	s.mux.Handle("GET /a/mqtt/ws-info", s.requireAdminSession(http.HandlerFunc(s.handleAdminMQTTWSInfo)))

	// Telegram bot admin: on/off + write-only token, chat allowlist,
	// pending-chat approval (in-product chat-id discovery), test send,
	// and the counter endpoint the page's auto-refresh polls.
	s.mux.Handle("GET /a/telegram", s.requireAdminSession(http.HandlerFunc(s.handleAdminTelegramGet)))
	s.mux.Handle("GET /a/telegram.json", s.requireAdminSession(http.HandlerFunc(s.handleAdminTelegramJSON)))
	s.mux.Handle("POST /a/telegram/settings", s.requireAdminSession(http.HandlerFunc(s.handleAdminTelegramSettingsPost)))
	s.mux.Handle("POST /a/telegram/chats", s.requireAdminSession(http.HandlerFunc(s.handleAdminTelegramChatAdd)))
	s.mux.Handle("POST /a/telegram/chats/{id}/delete", s.requireAdminSession(http.HandlerFunc(s.handleAdminTelegramChatDelete)))
	s.mux.Handle("POST /a/telegram/pending/{id}/approve", s.requireAdminSession(http.HandlerFunc(s.handleAdminTelegramApprove)))
	s.mux.Handle("POST /a/telegram/pending/{id}/reject", s.requireAdminSession(http.HandlerFunc(s.handleAdminTelegramReject)))
	s.mux.Handle("POST /a/telegram/test", s.requireAdminSession(http.HandlerFunc(s.handleAdminTelegramTestSend)))

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
	// Saison 20-E1: Protect camera list for the (upcoming, E5) per-viewer
	// camera multi-select UI. Mirrors /a/doors.json. Source =
	// streams.ListCameras; empty when no stream backend is configured.
	s.mux.Handle("GET /a/cameras.json", s.requireAdminSession(http.HandlerFunc(s.handleAdminCamerasJSON)))

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
	// Saison 19-39: per-setting tenant visibility ("dem Mieter anzeigen").
	// Binary (tenant_visible / admin_only); superseded by the three-level
	// /exposure below but kept for API/back-compat.
	s.mux.Handle("POST /a/viewers/{mac}/visibility", s.requireAdminSession(http.HandlerFunc(s.handleAdminViewerVisibility)))
	// Saison 20: three-level exposure per function (ausgeblendet / nur Admin /
	// fuer Mieter sichtbar) and template assignment. Both broadcast
	// config.changed for the one viewer; a template attaches live (no copy).
	s.mux.Handle("POST /a/viewers/{mac}/exposure", s.requireAdminSession(http.HandlerFunc(s.handleAdminViewerExposure)))
	s.mux.Handle("POST /a/viewers/{mac}/template", s.requireAdminSession(http.HandlerFunc(s.handleAdminViewerTemplate)))
	// Saison 19-32: admin-side door open from the viewer lists (per-row
	// "Tuer oeffnen"). Standby semantics; admin-trusted, no door authz.
	s.mux.Handle("POST /a/viewers/{mac}/unlock", s.requireAdminSession(http.HandlerFunc(s.handleAdminViewerUnlock)))
	s.mux.Handle("POST /a/viewers/{mac}/settings", s.requireAdminSession(http.HandlerFunc(s.handleAdminViewerSettings)))
	s.mux.Handle("POST /a/viewers/{mac}/password", s.requireAdminSession(http.HandlerFunc(s.handleAdminViewerPassword)))
	s.mux.Handle("POST /a/viewers/{mac}/regenerate-token", s.requireAdminSession(http.HandlerFunc(s.handleAdminViewerRegenerateToken)))

	// Benutzer-Seite: EINE Liste - die CARVILON-Benutzer (Master).
	// UA ist kein eigener Benutzer-Bestand, nur eine optionale
	// Verknuepfung pro Benutzer.
	s.mux.Handle("GET /a/users", s.requireAdminSession(http.HandlerFunc(s.handleAdminUsersList)))

	// Native CARVILON-Benutzer-CRUD + UA-Verknuepfung. Eigener
	// Pfad-Namensraum unter /a/users/carvilon/ (kollidiert nicht mit
	// der UA-Profil-Detailroute /a/users/{id}).
	s.mux.Handle("POST /a/users/carvilon", s.requireAdminSession(http.HandlerFunc(s.handleAdminNativeUserCreate)))
	s.mux.Handle("POST /a/users/carvilon/{id}/update", s.requireAdminSession(http.HandlerFunc(s.handleAdminNativeUserUpdate)))
	s.mux.Handle("POST /a/users/carvilon/{id}/activate", s.requireAdminSession(http.HandlerFunc(s.handleAdminNativeUserActivate)))
	s.mux.Handle("POST /a/users/carvilon/{id}/deactivate", s.requireAdminSession(http.HandlerFunc(s.handleAdminNativeUserDeactivate)))
	s.mux.Handle("POST /a/users/carvilon/{id}/delete", s.requireAdminSession(http.HandlerFunc(s.handleAdminNativeUserDelete)))
	s.mux.Handle("POST /a/users/carvilon/{id}/link", s.requireAdminSession(http.HandlerFunc(s.handleAdminNativeUserLink)))
	s.mux.Handle("POST /a/users/carvilon/{id}/unlink", s.requireAdminSession(http.HandlerFunc(s.handleAdminNativeUserUnlink)))

	// UA-Profile werden NICHT ueber uns verwaltet. Geblieben ist nur:
	// die Profil-Liste als JSON fuer den Viewer-Verknuepfungs-Dialog
	// (andere UA-Abhaengigkeit, bewusst unberuehrt) und der read-only
	// Profil-Blick, der aus den Viewer-Seiten verlinkt ist.
	s.mux.Handle("GET /a/users.json", s.requireAdminSession(http.HandlerFunc(s.handleAdminUsersListJSON)))
	s.mux.Handle("GET /a/users/{id}", s.requireAdminSession(http.HandlerFunc(s.handleAdminUsersDetail)))
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
