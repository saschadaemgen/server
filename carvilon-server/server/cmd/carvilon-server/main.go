package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	accesscarvilon "carvilon.local/server/internal/access/carvilon"
	"carvilon.local/server/internal/access/ua"
	"carvilon.local/server/internal/auth/admin"
	"carvilon.local/server/internal/auth/adminsession"
	"carvilon.local/server/internal/auth/loginaudit"
	"carvilon.local/server/internal/auth/ratelimit"
	"carvilon.local/server/internal/auth/session"
	"carvilon.local/server/internal/config"
	"carvilon.local/server/internal/console"
	"carvilon.local/server/internal/consolestore"
	"carvilon.local/server/internal/db"
	"carvilon.local/server/internal/designerstore"
	"carvilon.local/server/internal/dnssd"
	"carvilon.local/server/internal/doorbellcalls"
	"carvilon.local/server/internal/doorbellhub"
	"carvilon.local/server/internal/doorhistory"
	"carvilon.local/server/internal/egresstoken"
	"carvilon.local/server/internal/eventbus"
	"carvilon.local/server/internal/fcm"
	"carvilon.local/server/internal/featuregate"
	"carvilon.local/server/internal/gpio"
	"carvilon.local/server/internal/hostinfo"
	"carvilon.local/server/internal/httpserver"
	"carvilon.local/server/internal/logbuf"
	"carvilon.local/server/internal/mdns"
	"carvilon.local/server/internal/mideamonitor"
	"carvilon.local/server/internal/mideastore"
	"carvilon.local/server/internal/mqttbroker"
	"carvilon.local/server/internal/mqttstore"
	"carvilon.local/server/internal/nfc"
	"carvilon.local/server/internal/platformconfig"
	"carvilon.local/server/internal/protectapi"
	"carvilon.local/server/internal/protectmonitor"
	"carvilon.local/server/internal/publishtoken"
	"carvilon.local/server/internal/readerstore"
	"carvilon.local/server/internal/secrets"
	"carvilon.local/server/internal/sensorhistory"
	"carvilon.local/server/internal/shellystore"
	"carvilon.local/server/internal/sidechannel"
	"carvilon.local/server/internal/streampublish"
	"carvilon.local/server/internal/streams"
	"carvilon.local/server/internal/streamstore"
	"carvilon.local/server/internal/sysmetrics"
	"carvilon.local/server/internal/telegrambot"
	"carvilon.local/server/internal/telegramstore"
	"carvilon.local/server/internal/turnstore"
	"carvilon.local/server/internal/uaapi"
	"carvilon.local/server/internal/viewermanager"
	"carvilon.local/server/internal/weather"
)

func main() {
	role := flag.String("role", "edge",
		"process role: 'edge' (RPi: full local stack plus the side-channel "+
			"client) or 'cloud' (VPS: side-channel server only)")
	flag.Parse()

	// The stdout/journald handler stays the primary log sink; the logbuf
	// tee additionally retains recent entries in memory for the designer's
	// System Log tab (SSE), without altering the stdout output.
	logBuf := logbuf.New(1000)
	log := slog.New(logbuf.Tee(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}), logBuf))
	cfg := config.FromEnv()

	// One context for graceful shutdown, shared by whichever role
	// runs. SIGINT/SIGTERM cancels it; every subsystem goroutine and
	// the side-channel watch it.
	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	switch *role {
	case "edge":
		runEdge(ctx, log, logBuf, cfg)
	case "cloud":
		runCloud(ctx, log, cfg)
	default:
		log.Error("invalid -role (want 'edge' or 'cloud')", "role", *role)
		os.Exit(1)
	}
}

// shutdownWatchdogTimeout caps how long runEdge's cleanup (its defer chain:
// db, mqtt broker, telegram, nfc, console, mock manager) may take before the
// process force-exits. It must exceed the longest legitimate cleanup so it
// never fires on a healthy stop - viewerMgr.Shutdown already caps at ~5s, so
// 8s leaves margin. A subsystem Stop()/Close() that blocks past this (e.g. the
// broker writing a DISCONNECT to a half-open, dead-device MQTT socket, which
// can stall on the OS TCP write timeout for ~2 min) is force-capped here.
const shutdownWatchdogTimeout = 8 * time.Second

// runEdge boots the full local stack (the RPi role): config, db,
// auth, mock viewers, doorbell hub, HTTP/stream server, plus the
// ADDITIVE side-channel client. This is the historical main() body;
// nothing about the local behaviour changed, the side-channel client
// is layered on at the end and never gates startup.
func runEdge(ctx context.Context, log *slog.Logger, logBuf *logbuf.Buffer, cfg config.Config) {
	if err := cfg.Validate(); err != nil {
		log.Error("config invalid", "err", err)
		os.Exit(1)
	}
	if cfg.ServerIPv4 == "" {
		log.Warn("CARVILON_SERVER_IPV4 not set (legacy alias UNIFIX_SERVER_IPV4 also accepted); mock viewers will not be reachable by UDM")
	}

	// Detect usable GPIO once at startup (GPIO track T2). On a GPIO host
	// it registers the gpio: driver surface; elsewhere it stays silent and
	// nothing GPIO appears in the editor palette or runs.
	gpio.Probe(log)

	// Detect readable system-telemetry metrics once at startup (sys: float
	// driver). Only the metrics this host can actually read surface as
	// "system" source blocks; on a non-Linux host the category is absent.
	sysmetrics.Probe(log)

	// Detect PN532 NFC readers on the I2C buses once at startup (nfc:
	// driver, first step of the tags track). Only detected readers
	// surface as "NFC" source blocks; without one the category is absent.
	nfc.Probe(log)

	secretsSvc, err := secrets.New()
	if err != nil {
		log.Error("secrets init failed (set CARVILON_SECRETS_KEY; legacy alias UNIFIX_SECRETS_KEY also accepted; use cmd/genkey to generate one)",
			"err", err)
		os.Exit(1)
	}

	database, err := db.Open(cfg.DBPath)
	if err != nil {
		log.Error("db open failed", "err", err)
		os.Exit(1)
	}
	defer database.Close()

	platformCfg := platformconfig.New(database, secretsSvc)

	// Saison 13-02-FIX4-a: ein 32-Byte Pepper fuer Argon2id wird
	// beim ersten Boot generiert und ab dann persistent benutzt.
	// Der Master-Key (CARVILON_SECRETS_KEY; legacy alias
	// UNIFIX_SECRETS_KEY) verschluesselt den Pepper im
	// platform_config-Eintrag.
	if err := ensurePepper(context.Background(), platformCfg, log); err != nil {
		log.Error("ensure viewer pepper failed", "err", err)
		os.Exit(1)
	}

	pepperBridge := platformPepperBridge{cfg: platformCfg}
	adminSvc := admin.New(database, admin.WithPepper(pepperBridge))
	sessionSvc := session.New(database)
	adminSessionSvc := adminsession.New(database)
	auditSvc := loginaudit.New(database)
	viewerLimiter := ratelimit.New()
	adminLimiter := ratelimit.New()
	historyStore := doorhistory.NewSQLStore(database.DB)

	// MQTT broker (step 1). Device credentials + ACL rules live in
	// their own tables (migration 030); passwords are Argon2id-hashed
	// with the platform pepper, same as the admin/viewer accounts. The
	// broker is default-off and admin-toggled: no listener binds until
	// the operator enables it on /a/mqtt. The plaintext listener is
	// pinned to the LAN IP (never 0.0.0.0); TLS may face untrusted nets.
	mqttPepper := func(c context.Context) (string, error) {
		return platformCfg.GetSecret(c, platformconfig.KeyViewerPwPepper)
	}
	mqttStore := mqttstore.New(database.DB, mqttPepper)
	mqttSettings := loadMQTTSettings(ctx, platformCfg, cfg, log)
	mqttBroker := mqttbroker.New(mqttStore, mqttbroker.NewConsole(500), log,
		filepath.Dir(cfg.DBPath), mqttSettings)
	if err := mqttBroker.Start(ctx); err != nil {
		// A bind/cert failure must not abort the host; the admin page
		// surfaces the error and the operator can fix ports/TLS live.
		log.Error("mqtt broker start failed (continuing; fix via /a/mqtt)", "err", err)
	}
	defer mqttBroker.Stop()

	// Telegram bot: the first, deliberately fenced-in cloud function.
	// Default-off, outbound only to api.telegram.org, chat allowlist in
	// its own tables (migration 031), the bot token encrypted in
	// platform_config. A Telegram failure never touches the engine or
	// any other subsystem - start is non-fatal, the admin page shows why.
	telegramStore := telegramstore.New(database.DB)
	telegramBot := telegrambot.New(telegramStore, log, loadTelegramSettings(ctx, platformCfg, log))
	if err := telegramBot.Start(ctx); err != nil {
		log.Error("telegram bot start failed (continuing; fix via /a/telegram)", "err", err)
	}
	defer telegramBot.Stop()

	// Saison 18-10: TURN/STUN/ICE telemetry. The writer serialises all
	// SQLite writes (TURN events forwarded from the cloud over the
	// side-channel + the edge whipclient's ICE-state events) through one
	// goroutine and runs the 30-day retention purge; the snapshot holder
	// caches the latest cloud-pushed live snapshot for /a/turn.
	turnStore := turnstore.NewStore(database.DB)
	turnWriter := turnstore.NewWriter(turnStore, log.With("component", "turnstore"), turnstore.Options{})
	turnSnapshots := turnstore.NewSnapshotHolder()
	// S20: cache the latest cloud-pushed cloud-viewer snapshot (per-stream
	// WHEP consumer counts) for the admin dashboard, the egress mirror of
	// turnSnapshots. Stamped with the edge receive time on arrival; no
	// SQLite history (a consumer count is a live gauge, not an event log).
	streamSnapshots := streamstore.NewSnapshotHolder()
	go turnWriter.Run(ctx)

	viewerMgr := viewermanager.New(database, log, viewermanager.Options{
		StateDirBase: cfg.MockStateDir,
		ServerIPv4:   cfg.ServerIPv4,
	})

	if err := viewerMgr.LoadFromDB(ctx); err != nil {
		log.Error("mock manager load failed", "err", err)
		os.Exit(1)
	}
	defer func() {
		shutCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = viewerMgr.Shutdown(shutCtx)
	}()

	// Saison 13-03: zentrale Push-Quelle plus Anruf-Lifecycle.
	// EventBus wird vom HTTPServer und vom doorbellhub geteilt,
	// damit ESP-SSE und Web-SSE aus derselben Quelle gefuettert
	// werden. doorbellcalls speichert pro Anruf den CAS-Stand
	// fuer Multi-Viewer-Annehmen.
	eventBus := eventbus.New()
	callsSvc := doorbellcalls.New(database.DB)

	// Saison 17: additive FCM doorbell-push leg (edge role). The RPi
	// calls Google directly; this is NOT routed through the cloud /
	// side-channel. Built only when configured; a build failure
	// disables the leg without failing the edge (Grundregel: the
	// cloud is additive, the local doorbell flow is unaffected).
	fcmSender := buildFCMSender(ctx, log, cfg)

	hub := doorbellhub.NewWithOptions(viewerMgr, historyStore, log, doorbellhub.Options{
		Bus:       eventBus,
		Calls:     callsSvc,
		FCMSender: fcmSender,
		FCMTokens: viewerMgr,
	})
	go func() {
		if err := hub.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("doorbell hub stopped", "err", err)
		}
	}()

	// Background-Job: Login-Audit alle 6h aufraeumen (90d-Retention).
	go runAuditCleanup(ctx, auditSvc, log)

	// Build the UA client lazily: only if the operator has already
	// stored a base URL and token via the admin settings page.
	var uaClient *uaapi.Client
	baseURL, _ := platformCfg.Get(ctx, platformconfig.KeyUAAPIBaseURL)
	token, _ := platformCfg.GetSecret(ctx, platformconfig.KeyUAAPIToken)
	if baseURL != "" && token != "" {
		uaClient = uaapi.New(uaapi.Options{BaseURL: baseURL, Token: token})
		log.Info("ua api client configured", "base_url", baseURL)
	} else {
		log.Info("ua api not yet configured; admin must set base URL + token under /a/settings")
	}

	// Saison 21 - Protect Etappe 1: the UniFi Protect Integration
	// client, same lazy pattern as UA (host + X-API-KEY from the
	// encrypted platform_config; the key never reaches a log line).
	var protectClient *protectapi.Client
	protectURL, _ := platformCfg.Get(ctx, platformconfig.KeyProtectAPIBaseURL)
	protectKey, _ := platformCfg.GetSecret(ctx, platformconfig.KeyProtectAPIKey)
	if protectURL != "" && protectKey != "" {
		protectClient = protectapi.New(protectapi.Options{BaseURL: protectURL, APIKey: protectKey})
		// Deliberately no base_url attribute: the Protect briefing
		// forbids the configured host in any log line (the log also
		// reaches the admin System Log console via the logbuf tee).
		log.Info("protect api client configured")
	} else {
		log.Info("protect api not yet configured; admin can set host + api key under /a/settings")
	}

	// Saison 21 - Shelly Etappe 2: the device set lives in the shelly_devices
	// table (migration 038), the single source of truth for manual + mDNS-
	// discovered devices plus the sticky ignore list. Seed the table from the
	// Etappe-1 legacy address list exactly once, then build one client per
	// active device (LAN-guarded; neither addresses nor the password ever
	// reach a log line - only the device count does).
	shellyStore := shellystore.New(database.DB)
	httpserver.SeedShellyManualFromLegacy(ctx, shellyStore, platformCfg, log)
	// A provision that was in flight when the process last exited is not
	// resumed; clear any stale "provisioning" state so it shows as failed
	// (retryable) instead of hanging forever.
	if err := shellyStore.ResetStaleProvisioning(ctx); err != nil {
		log.Warn("shelly: reset stale provisioning state failed", "err", err)
	}
	// Midea Climate Controller (Etappe 1): the persistent device set +
	// AES-256-GCM credentials (migration 042) and the monitor that keeps
	// adopted devices connected + polled and proxies standard-profile control.
	// The monitor re-provisions the persisted active set on startup, so an
	// adopted device survives a server restart.
	mideaStore := mideastore.New(database.DB, secretsSvc)
	mideaMonitor := mideamonitor.New(mideaStore, log)
	shellyPassword, _ := platformCfg.GetSecret(ctx, platformconfig.KeyShellyPassword)
	shellyActive, err := shellyStore.ListActive(ctx)
	if err != nil {
		log.Warn("shelly: list active devices failed at startup", "err", err)
	}
	shellyClients := httpserver.BuildShellyFleet(shellyActive, shellyPassword)
	if len(shellyClients) > 0 {
		log.Info("shelly api clients configured", "devices", len(shellyClients))
	} else {
		log.Info("shelly api not yet configured; admin can add device addresses under /a/settings or let mDNS discovery find them")
	}

	// mDNS auto-discovery browsers. Gen2+ devices advertise _shelly._tcp;
	// Gen1 devices only announce a plain _http._tcp instance whose name the
	// strict allowlist classifies. Both are passive multicast listeners plus
	// active queries; they coexist with a system responder (avahi) via
	// address/port reuse and fall back to ephemeral-port active queries if
	// they cannot bind 5353. Opened always (near-zero passive load); the
	// coordinator only adopts + actively scans while Shelly is enabled.
	shellyBrowser := dnssd.Open(httpserver.ShellyServiceType, log)
	defer shellyBrowser.Close()
	shellyGen1Browser := dnssd.Open(httpserver.Shelly1ServiceType, log)
	defer shellyGen1Browser.Close()

	// access.UserStore-Adapter um den uaapi-Client. Nil-Client ist
	// erlaubt; der Adapter liefert dann access.ErrNotConfigured und
	// das Admin-UI zeigt einen Hinweis-Karten statt einer leeren
	// Liste.
	userStore := ua.New(uaClient)

	// CARVILONs eigene Benutzer (Migration 034). Immer verfuegbar - eine
	// lokale SQLite-Tabelle, unabhaengig von UA. Das ist die kanonische
	// Benutzer-Quelle; UA haengt optional dran.
	nativeUserStore := accesscarvilon.New(database.DB)

	// Saison 17-08: in-process stream server (carvilon_stream build
	// only). startInProcessStream is nil in the public build, so this
	// whole block is skipped there. When the hook runs and succeeds it
	// boots the in-process stream.Server (serving the local MJPEG/Offer
	// HTTP path on :8555) and hands back its StreamBackend for the
	// commercialBackend slot plus a (later) WHIP publisher. Setup
	// failure only logs and disables the stream subsystem; the edge core
	// (mocks, WS, MQTT, side-channel, FCM) keeps running (Grundregel).
	var inProcStreamPublisher streampublish.StreamPublisher
	if startInProcessStream != nil {
		backend, publisher, shutdown, err := startInProcessStream(ctx, log, cfg, viewerMgr, turnWriter)
		switch {
		case err != nil:
			log.Error("in-process stream setup failed; stream subsystem disabled", "err", err)
		case backend != nil:
			commercialBackend = backend
			inProcStreamPublisher = publisher
			if shutdown != nil {
				defer shutdown()
			}
			log.Info("in-process stream server enabled")
		}
	}

	// Stream-Backend zeigt auf die StreamBackend-Naht
	// (Saison 15-01). Praezedenz seit Saison 15-07:
	//
	//   1. commercialBackend (Build-Tag carvilon_stream; bindet den
	//      privaten carvilon-streaming-server). Im public-Build ist
	//      commercialBackend nil (siehe backend_default.go) und
	//      dieser Zweig faellt durch.
	//   2. CARVILON_STREAM_BACKEND_URL gesetzt -> transitional
	//      go2rtc-Client (S14-01).
	//   3. Sonst streams.Unconfigured() (503-Default), Handler
	//      degraden sauber per Configured()-Check.
	//
	// Das oeffentliche Repo importiert den privaten Server NIE
	// direkt; die Build-Tag-Naht ist die einzige Vertrags-Grenze.
	var streamBackend streams.StreamBackend = streams.Unconfigured()
	switch {
	case commercialBackend != nil:
		streamBackend = commercialBackend
		log.Info("stream backend configured", "source", "commercial wrapper (carvilon_stream build tag)")
	case cfg.StreamBackendURL != "":
		c, err := streams.New(cfg.StreamBackendURL)
		if err != nil {
			log.Error("stream backend init failed", "err", err)
			os.Exit(1)
		}
		streamBackend = c
		// Boot-Log mit der vom Briefing geforderten Wortlaut, damit
		// der Operator nach jedem systemctl restart sofort sieht ob
		// /esp/stream.mjpeg und /webviewer/stream.mjpeg funktionieren.
		log.Info("stream backend configured", "url", cfg.StreamBackendURL, "source", "go2rtc transitional")
	default:
		log.Warn("stream backend not configured: /esp/stream.mjpeg, /webviewer/stream.mjpeg and /webviewer/offer return 503")
	}

	// Saison 14-01b: weather-Backend (open-meteo) ist immer aktiv;
	// braucht keinen API-Key. Bei Internet-Ausfall liefert der
	// Cache stale snapshots bis zu 24h; danach blendet die UI den
	// Wetter-Bereich aus.
	weatherClient := weather.New()
	log.Info("weather backend configured", "provider", "open-meteo")

	// Saison 18-14: short-lived WHEP egress-token issuer. Optional: no
	// key -> nil issuer -> GET /webviewer/egress-token soft-503s (the
	// cloud egress is additive, no boot break). Validate already rejected
	// a malformed key at boot, so a decode error here is defensive only.
	var egressIssuer *egresstoken.Issuer
	if cfg.EgressTokenHMACKey != "" {
		key, err := cfg.DecodeEgressTokenHMACKey()
		if err != nil {
			log.Error("egress token key invalid; egress-token endpoint disabled", "err", err)
		} else {
			egressIssuer = egresstoken.NewIssuer(key)
			log.Info("egress token issuance enabled")
		}
	} else {
		log.Warn("egress token not configured: GET /webviewer/egress-token returns 503 " +
			"(set CARVILON_EGRESS_TOKEN_HMAC_KEY to enable)")
	}

	// Terminal dock session frame (Terminal-Track step 1). It owns the
	// live console sessions (local shell PTY / outbound SSH), caps
	// concurrency, and tears each down cleanly. CloseAll on shutdown ends
	// every open terminal so no shell/SSH process is orphaned.
	consoleMgr := console.NewManager(log)
	defer consoleMgr.CloseAll()

	// Reader registry (NFC-Track, device level). Every tag reader the
	// nfc driver detected at startup is auto-registered as a protected
	// component with an online row and a speaking auto-name; a reader
	// that is no longer present stays as an offline row instead of
	// vanishing. This is the component view (the Device Center row,
	// source "RPi") of the same detection the editor palette exposes as
	// logic blocks - one source, two views. A reader is NOT a graph in
	// the folder tree.
	designerStore := designerstore.New(database.DB)
	readerStore := readerstore.New(database.DB)
	platform := nfcPlatformTag()
	detectedReaders := make([]readerstore.Detected, 0, len(nfc.Readers()))
	for _, rd := range nfc.Readers() {
		detectedReaders = append(detectedReaders, readerstore.Detected{
			ID:       rd.Identity,
			Kind:     "nfc",
			Model:    rd.Model,
			Firmware: rd.Firmware,
			Bus:      rd.ID,
			Name:     readerAutoName(platform, "nfc", rd.Model, rd.ID),
		})
	}
	if err := readerStore.Sync(ctx, detectedReaders); err != nil {
		log.Error("reader registry sync failed (continuing; Device Center readers may be stale)", "err", err)
	}

	// The reader is infrastructure: one persistent poll goroutine per
	// reader runs from now until shutdown, feeding the registry's
	// last-seen tag continuously (no editor run required) and, while a
	// graph runs, that run's engine too. The tag observer is set BEFORE
	// the monitor starts so the very first read lands in the registry.
	nfc.SetTagObserver(func(id, uid string) {
		if err := readerStore.NoteTag(ctx, id, uid); err != nil {
			log.Warn("reader registry: note tag failed", "reader", id, "err", err)
		}
	})
	nfcMonitor := nfc.NewMonitor(log)
	defer nfcMonitor.Close()

	// Saison 21 - the capability-driven sensor readout editor modules + the
	// engine protect: source driver read from ONE persistent Protect sensor
	// poller (not the per-request Device Center calls). It re-derives its
	// client from platform_config each cycle (see protectSensorSource), so a
	// credential change or a disable takes effect without a restart and it
	// never races the server's hot-swapped protect client. Stops on ctx.
	// Sensor History H1 - the STORED path. The recorder taps the reading
	// stream (protectMonitor.OnReading below) and averages readings into
	// interval buckets in SQLite (migration 041), pruning each sensor's
	// retention. It is strictly decoupled from the LIVE path: Record is a
	// non-blocking hand-off, so recording never delays the real-time output
	// the editor block + cockpit + climate loop consume.
	sensorHistStore := sensorhistory.New(database.DB)
	sensorHistCfg := sensorhistory.NewConfigStore(database.DB)
	if err := sensorHistCfg.Load(ctx); err != nil {
		log.Warn("sensor history: recording config load failed (using defaults)", "err", err)
	}
	sensorHistRec := sensorhistory.NewRecorder(sensorhistory.RecorderConfig{
		Store:  sensorHistStore,
		Config: sensorHistCfg,
		Log:    log,
	})
	go sensorHistRec.Run(ctx)

	protectMonitor := protectmonitor.New(protectmonitor.Config{
		Source:    protectSensorSource(platformCfg),
		Log:       log,
		OnReading: sensorHistRec.Record,
	})
	go protectMonitor.Run(ctx)

	srv, err := httpserver.New(httpserver.Deps{
		Config:              cfg,
		Sessions:            sessionSvc,
		AdminSessions:       adminSessionSvc,
		ViewerManager:       viewerMgr,
		Admin:               adminSvc,
		PlatformConfig:      platformCfg,
		Audit:               auditSvc,
		ViewerLimiter:       viewerLimiter,
		AdminLimiter:        adminLimiter,
		UA:                  uaClient,
		Protect:             protectClient,
		Shelly:              shellyClients,
		ShellyStore:         shellyStore,
		ShellyDiscovery:     shellyBrowser,
		ShellyGen1Discovery: shellyGen1Browser,
		MideaStore:          mideaStore,
		MideaMonitor:        mideaMonitor,
		UserStore:           userStore,
		NativeUsers:         nativeUserStore,
		Hub:                 hub,
		History:             historyStore,
		TURNStore:           turnStore,
		TURNSnapshots:       turnSnapshots,
		StreamSnapshots:     streamSnapshots,
		EventBus:            eventBus,
		DoorbellCalls:       callsSvc,
		Streams:             streamBackend,
		Weather:             weatherClient,
		EgressIssuer:        egressIssuer,
		Features:            featuregate.NewStore(database.DB),
		MQTT:                mqttBroker,
		MQTTStore:           mqttStore,
		Telegram:            telegramBot,
		TelegramStore:       telegramStore,
		DesignerStore:       designerStore,
		ReaderStore:         readerStore,
		NFCMonitor:          nfcMonitor,
		ProtectMonitor:      protectMonitor,
		SensorHistory:       sensorHistStore,
		SensorHistoryConfig: sensorHistCfg,
		LogBuffer:           logBuf,
		Console:             consoleMgr,
		ConsoleStore:        consolestore.New(database.DB, secretsSvc),
		Log:                 log,
	})
	if err != nil {
		log.Error("httpserver init failed", "err", err)
		os.Exit(1)
	}

	// Shelly Etappe 2: run the mDNS auto-discovery coordinator for the
	// process lifetime. It consumes the browser's announcements, auto-adopts
	// Gen2+ devices into the set (while Shelly is enabled) and honours the
	// sticky ignore list. Stops when ctx is cancelled.
	go srv.RunShellyDiscovery(ctx)

	// Midea Climate Controller (Etappe 1): keep adopted devices connected +
	// polled for the process lifetime, re-provisioning the persisted set on
	// startup. Stops when ctx is cancelled.
	go srv.RunMideaMonitor(ctx)

	// Unified automatic discovery: a periodic, LAN-guarded background sweep of
	// every discovery source (Shelly active subnet + Midea broadcast; Shelly
	// mDNS has its own backstop) so newly powered devices appear on their own.
	go srv.RunPeriodicDiscovery(ctx)

	// mDNS-Advertisement (Saison 13-02-FIX4-d). Adoptierte
	// ESP-Viewer finden den Server via _carvilon._tcp.local statt
	// einer haendischen IP-Konfiguration. Skip wenn keine IP
	// gesetzt ist; dann waere die Antwort sowieso nutzlos.
	mdnsSvc, err := startMDNSIfPossible(cfg.ServerIPv4, cfg.ListenAddr, log)
	if err != nil {
		log.Warn("mdns advertisement skipped", "err", err)
	}
	defer func() {
		if mdnsSvc != nil {
			_ = mdnsSvc.Close()
		}
	}()

	log.Info("carvilon-server starting",
		"addr", cfg.ListenAddr,
		"dev_mode", cfg.DevMode,
		"db", cfg.DBPath,
		"mock_state_dir", cfg.MockStateDir,
		"server_ipv4", cfg.ServerIPv4,
		"mdns_active", mdnsSvc != nil,
	)

	// Saison 17: additive side-channel client to the cloud. Started
	// only when fully configured; runs in its own goroutine and its
	// failures only trigger reconnects - it never blocks or delays
	// the edge (Grundregel: LAN works without the cloud).
	scClient := startSidechannelClient(ctx, log, cfg, viewerMgr, egressIssuer, srv, inProcStreamPublisher, turnWriter, turnSnapshots, streamSnapshots)
	if scClient != nil {
		// Wire the cloud link as the ICE source for GET /webviewer/stream-start
		// (Saison 19). Set before ListenAndServe below, so the serving
		// goroutines see it without locking. nil client -> stream-start 503s.
		srv.SetICERequester(scClient)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
		// Bound the whole cleanup path. runEdge is about to return and run
		// its defer chain; a subsystem Stop()/Close() that blocks (the
		// broker writing to a half-open dead-device socket is the known
		// hazard) would stall the chain and hang `systemctl stop` until
		// systemd's TimeoutStopSec SIGKILLs us (~2 min). This is a plain
		// goroutine, NOT a defer: a blocked defer would run before a
		// deferred watchdog could, so only an independent goroutine started
		// BEFORE the defers can cap total shutdown time. If cleanup finishes
		// first, the process exits normally (0) before this ever fires.
		go func() {
			time.Sleep(shutdownWatchdogTimeout)
			log.Warn("shutdown watchdog fired - forcing exit", "after", shutdownWatchdogTimeout)
			os.Exit(0)
		}()
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server stopped", "err", err)
			os.Exit(1)
		}
	}
}

// runCloud boots the minimal cloud role (the VPS): only the
// side-channel server. No db, no mock viewers, no doorbell hub, no
// edge HTTP routes - the VPS has no UDM and no mocks. It validates
// only the cloud-relevant config (ValidateCloud) and blocks on the
// side-channel server until ctx is cancelled.
func runCloud(ctx context.Context, log *slog.Logger, cfg config.Config) {
	if err := cfg.ValidateCloud(); err != nil {
		log.Error("config invalid for cloud role", "err", err)
		os.Exit(1)
	}
	srv, err := sidechannel.NewServer(sidechannel.ServerOptions{
		ListenAddr: cfg.SidechannelListenAddr,
		CACertPath: cfg.SidechannelCACert,
		ServerCert: cfg.SidechannelServerCert,
		ServerKey:  cfg.SidechannelServerKey,
		Log:        log.With("component", "sidechannel-server"),
	})
	if err != nil {
		log.Error("sidechannel server init failed", "err", err)
		os.Exit(1)
	}
	// Interim trigger: a localhost HTTP hook the future stream-cloud
	// layer (or a manual curl on the VPS) uses to ask the edge to
	// publish, until the real WHEP-subscriber trigger replaces it. Runs
	// in its own goroutine; srv.Run blocks below. Disabled unless the
	// addr is configured.
	startInterimRequestPublishHook(ctx, log, cfg, srv)

	// Saison 19-11: public cloud control endpoint (signal host) so a remote
	// subscriber can fetch the stream-start bundle the CGNAT'd edge cannot
	// serve directly. Opt-in (CARVILON_SIGNAL_PUBLIC_ADDR); relays the viewer
	// Bearer to the edge over the side-channel and assembles cloud-side.
	startSignalControlListener(ctx, log, cfg, srv)

	// Saison 18-04: in-process WHIP/WHEP stream server (carvilon_stream
	// build only). startInProcessCloudStream is nil in the public build,
	// so this is skipped there; the combined binary runs the stream
	// in-process on the VPS instead of a separate stream-cloud binary.
	// Soft gate: unset WHIP config logs and runs side-channel-only. Setup
	// failure only logs and disables the stream subsystem; the
	// side-channel keeps running (Grundregel), symmetric to runEdge.
	if startInProcessCloudStream != nil {
		shutdown, err := startInProcessCloudStream(ctx, log, cfg, srv)
		switch {
		case err != nil:
			log.Error("in-process cloud stream setup failed; stream subsystem disabled", "err", err)
		case shutdown != nil:
			defer shutdown()
		}
	}

	log.Info("carvilon-server starting",
		"role", "cloud",
		"sidechannel_addr", cfg.SidechannelListenAddr,
	)
	if err := srv.Run(ctx); err != nil {
		log.Error("sidechannel server stopped", "err", err)
		os.Exit(1)
	}
	log.Info("cloud role shut down")
}

// startInterimRequestPublishHook starts a localhost-only, no-auth HTTP
// endpoint that triggers a request_publish to the connected edge(s).
//
// TODO S17-stream-integration: this is the interim test naht. The real
// trigger is a WHEP subscriber arriving at the stream-cloud layer on
// the VPS; that layer will call Server.RequestPublish in-process (or
// hit this same endpoint) once it docks. Until then this lets a curl on
// the VPS drive a cloud -> edge request_publish over the live link.
func startInterimRequestPublishHook(ctx context.Context, log *slog.Logger, cfg config.Config, srv *sidechannel.Server) {
	if cfg.SidechannelInternalAddr == "" {
		log.Info("sidechannel interim request-publish hook disabled (set CARVILON_SIDECHANNEL_INTERNAL_ADDR to enable)")
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/request-publish", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			StreamID string `json:"stream_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.StreamID == "" {
			http.Error(w, "stream_id required", http.StatusBadRequest)
			return
		}
		n := srv.RequestPublish(r.Context(), body.StreamID)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"requested": body.StreamID, "edges": n})
	})
	hookSrv := &http.Server{
		Addr:              cfg.SidechannelInternalAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		log.Warn("sidechannel interim request-publish hook listening (localhost, NO AUTH, interim)",
			"addr", cfg.SidechannelInternalAddr)
		if err := hookSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("interim request-publish hook stopped", "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		sh, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = hookSrv.Shutdown(sh)
	}()
}

// startSidechannelClient launches the edge-side cloud link IF it is
// fully configured. The link is ADDITIVE: an unconfigured or
// misconfigured side-channel never blocks or fails the edge - it is
// logged and skipped, and runtime failures only trigger reconnects
// inside the client goroutine.
// publisher is the edge-side video push surface (the WHIP client in the
// carvilon_stream build, supplied by the in-process stream setup). When
// nil - the public build, or an unconfigured/failed in-process stream -
// it falls back to the no-op publisher, so a request_publish issues a
// token but pushes nothing.
// It returns the running *sidechannel.Client so the caller can wire it as the
// httpserver's ICE source (the stream-start bundle), or nil when the link is
// unconfigured or failed to init - in which case stream-start 503s.
func startSidechannelClient(ctx context.Context, log *slog.Logger, cfg config.Config, viewerMgr *viewermanager.Manager, egressIssuer *egresstoken.Issuer, srv *httpserver.Server, publisher streampublish.StreamPublisher, turnWriter *turnstore.Writer, turnSnapshots *turnstore.SnapshotHolder, streamSnapshots *streamstore.SnapshotHolder) *sidechannel.Client {
	if !cfg.SidechannelClientConfigured() {
		log.Info("sidechannel client not configured; running LAN-only (cloud link is additive)")
		return nil
	}
	if publisher == nil {
		publisher = streamPublisher(log)
	}

	// Publish-token signing key from CARVILON_PUBLISH_TOKEN_HMAC_KEY
	// (its own env, NOT a master-key subkey, so the stream-cloud layer
	// can verify with the same key). Validate() already enforces this
	// once DIAL_URL is set; stay defensive and skip the cloud link
	// rather than crash if it is somehow invalid (additive rule).
	hmacKey, err := cfg.DecodePublishTokenHMACKey()
	if err != nil {
		log.Error("publish-token hmac key invalid; cloud link disabled", "err", err)
		return nil
	}

	// Edge publish controller: on request_publish from the cloud it
	// authorises the stream against the viewer manager, issues a
	// publish token, and kicks the StreamPublisher (no-op until the
	// stream layer docks). All of this is decoupled from the local LAN
	// flow - a cloud outage cannot stall the edge.
	edgePub := &sidechannel.EdgePublisher{
		Authorize: func(streamID string) bool {
			_, err := viewerMgr.GetViewerInfo(ctx, streamID)
			return err == nil
		},
		Issuer:       publishtoken.NewIssuer(hmacKey, 5*time.Minute),
		Publisher:    publisher,
		CloudWhipURL: cfg.SidechannelCloudWhipURL,
		Log:          log.With("component", "sidechannel-publish"),
	}

	client, err := sidechannel.NewClient(sidechannel.ClientOptions{
		URL:              cfg.SidechannelDialURL,
		CACertPath:       cfg.SidechannelCACert,
		ClientCert:       cfg.SidechannelClientCert,
		ClientKey:        cfg.SidechannelClientKey,
		Log:              log.With("component", "sidechannel-client"),
		OnRequestPublish: edgePub.HandleRequestPublish,
		// S20: the cloud reports the last WHEP subscriber for a stream left;
		// stop the publish bridge so the profile row falls back to idle. This
		// is the missing production caller of EdgePublisher.StopPublish - the
		// symmetric counterpart to OnRequestPublish above.
		OnRequestStop: func(streamID string) { edgePub.StopPublish(streamID, sidechannel.ReasonNoSubscribers) },
		// Saison 18-10: persist cloud-forwarded TURN telemetry and cache
		// the latest live snapshot (stamped with the edge receive time)
		// for the /a/turn admin page.
		OnTURNEvent: turnWriter.SubmitEvent,
		OnTURNStats: func(s turnstore.Snapshot) { turnSnapshots.Set(s, time.Now()) },
		// S20: cache the cloud-viewer snapshot stamped with the edge receive
		// time for the admin dashboard (egress mirror of OnTURNStats).
		OnStreamStats: func(s streamstore.Snapshot) { streamSnapshots.Set(s, time.Now()) },
		// Saison 19-11: answer cloud bundle_request (the remote stream-start
		// relay). resolveViewer maps the Bearer to a viewer MAC; the egress
		// issuer mints the sid-bound token. These are the two edge-only parts;
		// the cloud assembles the rest. nil egress issuer -> reject (cloud 401).
		OnBundleRequest: func(credential string) (string, string, int, string, error) {
			mac, rerr := resolveViewer(ctx, viewerMgr, credential)
			if rerr != nil {
				return "", "", 0, "", rerr
			}
			if egressIssuer == nil {
				return "", "", 0, "", errors.New("egress issuer not configured")
			}
			tok, ierr := egressIssuer.Issue(mac)
			if ierr != nil {
				return "", "", 0, "", ierr
			}
			// Saison 19-41 (Fall B): also supply the LAN-direct WHEP URL so the
			// cloud can put it in the bundle. Built edge-side via the S19-35
			// helper (profile-keyed, Flagge A); empty when LAN-WHEP is off ->
			// omitted from the bundle, app uses the cloud path. Best-effort: a
			// viewer-info miss just yields no edge URL (the bundle still works).
			edgeURL := ""
			if info, gerr := viewerMgr.GetViewerInfo(ctx, mac); gerr == nil {
				edgeURL, _ = httpserver.EdgeWHEPURL(cfg, info.ResolveStreamProfile())
			}
			return mac, tok, int(egresstoken.TTL.Seconds()), edgeURL, nil
		},
		// Saison 19-27: run a relayed control call (http_request) through the
		// edge's OWN httpserver mux and frame the captured response back. The
		// edge stays the auth authority (requireViewerAuth runs inside
		// ServeRelayed); a relay/build failure crosses back as an error (-> the
		// cloud 503s).
		OnHTTPRequest: func(rctx context.Context, req sidechannel.RelayRequest) (sidechannel.RelayResponse, error) {
			status, header, body, serr := srv.ServeRelayed(rctx, req.Method, req.Path, req.RawQuery, req.Header, req.Body)
			if serr != nil {
				return sidechannel.RelayResponse{}, serr
			}
			return sidechannel.RelayResponse{Status: status, Header: header, Body: body}, nil
		},
	})
	if err != nil {
		// A misconfigured side-channel must NOT take down the edge.
		log.Error("sidechannel client init failed; continuing without cloud link", "err", err)
		return nil
	}
	edgePub.Send = client.Send

	go func() {
		if err := client.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("sidechannel client stopped", "err", err)
		}
	}()
	log.Info("sidechannel client started",
		"url", cfg.SidechannelDialURL,
		"cloud_whip_url", cfg.SidechannelCloudWhipURL,
	)
	return client
}

// resolveViewer maps a raw Bearer device_token to a viewer MAC, reusing the
// exact lookup requireViewerAuth's Bearer path uses (viewers WHERE type IN
// ('esp','android')). It is the non-HTTP half of the viewer auth, for the
// cloud bundle RPC (OnBundleRequest). Bearer-only on purpose: Android
// authenticates with a device_token, not the browser session cookie.
func resolveViewer(ctx context.Context, viewerMgr *viewermanager.Manager, bearer string) (string, error) {
	return viewerMgr.LookupDeviceMACByToken(ctx, bearer)
}

// startSignalControlListener stands up the public cloud control endpoint
// (Saison 19-11): GET /webviewer/stream-start on a carvilon-owned HTTPS
// listener with the signal cert (path kept identical to the LAN edge so the
// app only swaps the base host, S19-09/S19-24), so a remote (Android)
// subscriber - which cannot reach the
// CGNAT'd edge - fetches the stream-start bundle via the cloud. The cloud
// relays the viewer Bearer to the edge over the side-channel (RequestBundle),
// then assembles the bundle from its own ICE mint + WHEP base (AssembleBundle).
// Opt-in: disabled unless CARVILON_SIGNAL_PUBLIC_ADDR is set. Runs in its own
// goroutine; a failure only logs (the side-channel keeps running, Grundregel).
// Pure carvilon - no stream import.
func startSignalControlListener(ctx context.Context, log *slog.Logger, cfg config.Config, srv *sidechannel.Server) {
	if cfg.SignalPublicAddr == "" {
		log.Info("signal control endpoint disabled (set CARVILON_SIGNAL_PUBLIC_ADDR to enable)")
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /webviewer/stream-start", func(w http.ResponseWriter, r *http.Request) {
		const bearerPrefix = "Bearer "
		authz := r.Header.Get("Authorization")
		if !strings.HasPrefix(authz, bearerPrefix) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		bearer := strings.TrimPrefix(authz, bearerPrefix)

		// Relay to the edge: it resolves the viewer + mints the egress token.
		// Auth-fail -> 401; no edge / timeout / link down -> 503.
		mac, egressToken, expiresIn, edgeWHEPURL, err := srv.RequestBundle(r.Context(), bearer)
		if err != nil {
			if errors.Is(err, sidechannel.ErrBundleAuth) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
			} else {
				http.Error(w, "stream start unavailable", http.StatusServiceUnavailable)
			}
			return
		}

		// Assemble cloud-side (the single cloud assembler): ICE + WHEP URL.
		bundle := srv.AssembleBundle(mac, egressToken, expiresIn, edgeWHEPURL)
		if bundle.WHEPURL == "" {
			// Signal endpoint up but no public WHEP base configured -> the
			// bundle has no subscribe target. Decline rather than hand out a
			// useless bundle.
			log.Warn("signal stream-start: assembled bundle has no whep_url (CARVILON_WHEP_PUBLIC_* not set)", "viewer_mac", mac)
			http.Error(w, "stream start not configured", http.StatusServiceUnavailable)
			return
		}
		log.Info("signal stream-start bundle issued", "viewer_mac", mac, "ice_servers", len(bundle.ICEServers))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(bundle)
	})

	// Saison 19-27: generic control relay. The allowlisted /webviewer/* control
	// calls execute on the EDGE (via the side-channel); the cloud is a dumb
	// pipe. EXPLICIT allowlist - NEVER a blanket /webviewer/* prefix: SSE
	// (/events) and MJPEG (/stream.mjpeg) would hang the edge ResponseRecorder
	// forever, and the HTML UI, /offer, egress-token, logout and stream-start
	// are excluded too (stream-start runs over B-Strich above).
	const maxRelayBody = 64 * 1024
	relay := func(w http.ResponseWriter, r *http.Request) {
		body, rerr := io.ReadAll(io.LimitReader(r.Body, maxRelayBody))
		if rerr != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		// Curate the request headers: only Authorization + Content-Type cross.
		header := map[string]string{}
		if a := r.Header.Get("Authorization"); a != "" {
			header["Authorization"] = a
		}
		if ct := r.Header.Get("Content-Type"); ct != "" {
			header["Content-Type"] = ct
		}
		resp, rerr := srv.RelayHTTP(r.Context(), sidechannel.RelayRequest{
			Method:   r.Method,
			Path:     r.URL.Path, // full path incl. the {door_id} segment
			RawQuery: r.URL.RawQuery,
			Header:   header,
			Body:     body,
		})
		if rerr != nil {
			// Relay mechanism failure (no edge / timeout / edge could not run) -
			// NOT a viewer-auth failure (the edge returns that as a 401 in
			// resp.Status). Detail to the log only.
			log.Warn("signal control relay failed", "method", r.Method, "path", r.URL.Path, "err", rerr)
			http.Error(w, "control endpoint unavailable", http.StatusServiceUnavailable)
			return
		}
		for k, v := range resp.Header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(resp.Status)
		_, _ = w.Write(resp.Body)
	}
	mux.HandleFunc("GET /webviewer/doors", relay)         // Saison 19-30: assigned-door list (small JSON)
	mux.HandleFunc("GET /webviewer/settings.json", relay) // Saison 19-37: config.changed refetch (small JSON)
	mux.HandleFunc("POST /webviewer/answer", relay)
	mux.HandleFunc("POST /webviewer/reject", relay)
	mux.HandleFunc("POST /webviewer/end-call", relay)
	mux.HandleFunc("POST /webviewer/doors/{door_id}/unlock", relay)
	mux.HandleFunc("POST /webviewer/fcm-token", relay)
	mux.HandleFunc("DELETE /webviewer/fcm-token", relay)

	hsrv := &http.Server{
		Addr:              cfg.SignalPublicAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		log.Info("signal control endpoint listening", "addr", cfg.SignalPublicAddr, "host", cfg.SignalPublicHost)
		if err := hsrv.ListenAndServeTLS(cfg.SignalPublicCert, cfg.SignalPublicKey); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("signal control endpoint stopped", "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		sh, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = hsrv.Shutdown(sh)
	}()
}

// streamPublisher selects the edge-side video publisher handed to the
// EdgePublisher. Today it is ALWAYS the no-op (logs, pushes nothing),
// so runtime behaviour is unchanged. The signatures of the real client
// already match streampublish.StreamPublisher, so the swap is contained
// to this one function.
//
// TODO S17-05 / Stream-S2-04b: return the real WHIP client once the
// carvilon.local/stream/whipclient package exists, e.g.
//
//	c, err := whipclient.New(whipclient.Config{ /* ... */ })
//	if err != nil { log.Error(...); return streampublish.NewNoop(log) }
//	return c
//
// The replace + require for carvilon.local/stream are already in
// server/go.mod; only the import + this body change (and the real
// import will require bumping the go directives to 1.26.1, since the
// stream module is on go 1.26.1).
func streamPublisher(log *slog.Logger) streampublish.StreamPublisher {
	return streampublish.NewNoop(log)
}

// buildFCMSender constructs the edge FCM doorbell-push sender, or
// returns nil (disabled) when FCM is not configured. A build error is
// logged and also yields nil: FCM stays off but the edge runs normally
// (Grundregel: the cloud is additive). The return type is the hub's
// interface so a nil result is a true nil interface the hub nil-checks
// - never a typed-nil.
func buildFCMSender(ctx context.Context, log *slog.Logger, cfg config.Config) doorbellhub.FCMPushSender {
	if !cfg.FCMEnabled() {
		log.Info("fcm doorbell push disabled (set CARVILON_FCM_SERVICE_ACCOUNT_JSON + CARVILON_FCM_PROJECT_ID to enable)")
		return nil
	}
	sender, err := fcm.NewSender(ctx, cfg.FCMServiceAccountJSON, cfg.FCMProjectID)
	if err != nil {
		log.Error("fcm sender init failed; doorbell push disabled", "err", err)
		return nil
	}
	if sender == nil {
		// Defensive: FCMEnabled() guarantees a non-empty path, so
		// NewSender does not return the disabled (nil,nil) case here.
		return nil
	}
	log.Info("fcm doorbell push configured", "project_id", cfg.FCMProjectID)
	return sender
}

// platformPepperBridge adaptiert *platformconfig.Service an das
// admin.PepperSource-Interface ohne dass das admin-Paket einen
// platformconfig-Import braucht.
type platformPepperBridge struct {
	cfg *platformconfig.Service
}

func (b platformPepperBridge) GetPepper(ctx context.Context) (string, error) {
	return b.cfg.GetSecret(ctx, platformconfig.KeyViewerPwPepper)
}

// ensurePepper sorgt dafuer dass der Argon2id-Pepper-Eintrag
// existiert. Wenn nicht: 32 Bytes crypto/rand erzeugen, hex-codiert
// als Klartext-String dem secrets-Service uebergeben (der hex-string
// wird als Pepper benutzt; AES-256-GCM verschluesselt ihn).
func ensurePepper(ctx context.Context, cfg *platformconfig.Service, log *slog.Logger) error {
	existing, err := cfg.GetSecret(ctx, platformconfig.KeyViewerPwPepper)
	if err == nil && existing != "" {
		return nil
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	pepper := hex.EncodeToString(raw)
	if err := cfg.SetSecret(ctx, platformconfig.KeyViewerPwPepper, pepper); err != nil {
		return err
	}
	log.Info("viewer password pepper generated and stored")
	return nil
}

// nfcPlatformTag is the "Platform" part of a reader's speaking auto-name:
// "RPi" on a Raspberry Pi, "SBC" on another device-tree board, "Host"
// elsewhere (dev machine / VPS). Derived from the device-tree model, not
// board-hardcoded.
func nfcPlatformTag() string {
	model := strings.ToLower(hostinfo.Detect().Model)
	switch {
	case strings.Contains(model, "raspberry pi"):
		return "RPi"
	case model != "":
		return "SBC"
	default:
		return "Host"
	}
}

// readerAutoName builds a reader's speaking auto-name in the scheme
// Platform-Function-Model (Bus), e.g. "RPi-NFC-PN532 (I2C-1)". It is
// future-open: a different model or bus yields a different name, and a
// second reader type just passes a different kind/model.
func readerAutoName(platform, kind, model, bus string) string {
	return fmt.Sprintf("%s-%s-%s (%s)", platform, strings.ToUpper(kind), model, strings.ToUpper(bus))
}

// loadMQTTSettings assembles the broker's runtime settings from the
// admin-tunable platform_config keys, with safe defaults: broker
// off, plaintext 1883 bound to the LAN IPv4 (never 0.0.0.0), TLS
// 8883. A read error degrades to the default rather than failing the
// boot - the admin page can correct it live.
func loadMQTTSettings(ctx context.Context, pc *platformconfig.Service, cfg config.Config, log *slog.Logger) mqttbroker.Settings {
	get := func(key string) string {
		v, err := pc.Get(ctx, key)
		if err != nil {
			log.Warn("mqtt setting read failed; using default", "key", key, "err", err)
			return ""
		}
		return v
	}
	port := func(key string, def int) int {
		if v := get(key); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
				return n
			}
		}
		return def
	}
	return mqttbroker.Settings{
		Enabled:  get(platformconfig.KeyMQTTEnabled) == "1",
		LANHost:  cfg.ServerIPv4, // LAN-only plaintext bind; empty -> loopback
		TCPPort:  port(platformconfig.KeyMQTTTCPPort, 1883),
		TLSHost:  "", // all interfaces: TLS may face untrusted networks
		TLSPort:  port(platformconfig.KeyMQTTTLSPort, 8883),
		CertFile: get(platformconfig.KeyMQTTCertFile),
		KeyFile:  get(platformconfig.KeyMQTTKeyFile),
		// WebSocket listener for the browser console: LAN-bound (uses the
		// same LANHost). wss when the admin surface is served over TLS
		// (not DevMode), so an HTTPS admin page has no mixed-content block.
		WSEnabled: get(platformconfig.KeyMQTTWSEnabled) == "1",
		WSPort:    port(platformconfig.KeyMQTTWSPort, 8083),
		WSUseTLS:  !cfg.DevMode,
	}
}

// loadTelegramSettings assembles the bot's runtime settings from the
// admin-tunable platform_config keys: enabled flag (plaintext) and the
// bot token (encrypted, GetSecret). Safe defaults: bot off, no token.
// A read error degrades to the default rather than failing the boot -
// the admin page can correct it live. The API base is never
// configurable in production (api.telegram.org is the one permitted
// outbound target); tests construct the Manager directly.
func loadTelegramSettings(ctx context.Context, pc *platformconfig.Service, log *slog.Logger) telegrambot.Settings {
	enabled, err := pc.Get(ctx, platformconfig.KeyTelegramEnabled)
	if err != nil {
		log.Warn("telegram setting read failed; using default (off)", "key", platformconfig.KeyTelegramEnabled, "err", err)
	}
	token, err := pc.GetSecret(ctx, platformconfig.KeyTelegramBotToken)
	if err != nil {
		log.Warn("telegram token read failed; bot stays down until re-set", "err", err)
	}
	return telegrambot.Settings{
		Enabled: enabled == "1",
		Token:   token,
	}
}

// startMDNSIfPossible parses listenAddr (":8080" or "0.0.0.0:8080")
// for the port and starts an mDNS advertisement. If serverIPv4 is
// empty (no CARVILON_SERVER_IPV4 set; legacy alias UNIFIX_SERVER_IPV4
// also accepted), returns (nil, nil) - the caller logs a warning
// and continues without mDNS.
func startMDNSIfPossible(serverIPv4, listenAddr string, log *slog.Logger) (*mdns.Service, error) {
	if serverIPv4 == "" {
		return nil, errors.New("CARVILON_SERVER_IPV4 not set")
	}
	_, portStr, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port == 0 {
		return nil, errors.New("listen addr port unparseable")
	}
	svc, err := mdns.Start(serverIPv4, port)
	if err != nil {
		return nil, err
	}
	log.Info("mdns advertising", "service", mdns.ServiceName, "instance", mdns.InstanceName, "ip", serverIPv4, "port", port)
	return svc, nil
}

// runAuditCleanup laeuft alle 6h und loescht login_audit-Zeilen
// aelter als die Default-Retention (90d).
func runAuditCleanup(ctx context.Context, audit *loginaudit.Service, log *slog.Logger) {
	tick := time.NewTicker(6 * time.Hour)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			n, err := audit.Cleanup(ctx, loginaudit.DefaultRetention)
			if err != nil {
				log.Warn("login_audit cleanup failed", "err", err)
				continue
			}
			if n > 0 {
				log.Info("login_audit cleanup removed", "count", n)
			}
		}
	}
}

// protectSensorSource returns the current Protect sensor source for the
// persistent poller, re-derived from platform_config on each poll so a
// credential change or a disable takes effect without a restart (and
// without racing the server's hot-swapped protect client, which the poller
// deliberately does not touch). It rebuilds the *protectapi.Client only when
// the (base URL, api key) pair actually changes; a disabled or unconfigured
// Protect returns nil, which the monitor reads as "no sensors". The key
// never reaches a log line here - it only flows into protectapi.New.
func protectSensorSource(pc *platformconfig.Service) func() protectmonitor.SensorSource {
	var (
		mu      sync.Mutex
		lastURL string
		lastKey string
		cached  *protectapi.Client
	)
	return func() protectmonitor.SensorSource {
		ctx := context.Background()
		if v, err := pc.Get(ctx, platformconfig.KeyProtectEnabled); err == nil && v == "0" {
			return nil // explicitly disabled
		}
		url, _ := pc.Get(ctx, platformconfig.KeyProtectAPIBaseURL)
		key, _ := pc.GetSecret(ctx, platformconfig.KeyProtectAPIKey)
		if url == "" || key == "" {
			return nil // not configured
		}
		mu.Lock()
		defer mu.Unlock()
		if cached == nil || url != lastURL || key != lastKey {
			cached = protectapi.New(protectapi.Options{BaseURL: url, APIKey: key})
			lastURL, lastKey = url, key
		}
		return cached
	}
}
