package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"carvilon.local/server/internal/access/ua"
	"carvilon.local/server/internal/auth/admin"
	"carvilon.local/server/internal/auth/adminsession"
	"carvilon.local/server/internal/auth/loginaudit"
	"carvilon.local/server/internal/auth/ratelimit"
	"carvilon.local/server/internal/auth/session"
	"carvilon.local/server/internal/config"
	"carvilon.local/server/internal/db"
	"carvilon.local/server/internal/doorbellcalls"
	"carvilon.local/server/internal/doorbellhub"
	"carvilon.local/server/internal/doorhistory"
	"carvilon.local/server/internal/eventbus"
	"carvilon.local/server/internal/httpserver"
	"carvilon.local/server/internal/mdns"
	"carvilon.local/server/internal/mockmanager"
	"carvilon.local/server/internal/platformconfig"
	"carvilon.local/server/internal/secrets"
	"carvilon.local/server/internal/streams"
	"carvilon.local/server/internal/uaapi"
	"carvilon.local/server/internal/weather"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg := config.FromEnv()
	if err := cfg.Validate(); err != nil {
		log.Error("config invalid", "err", err)
		os.Exit(1)
	}
	if cfg.ServerIPv4 == "" {
		log.Warn("UNIFIX_SERVER_IPV4 not set; mock viewers will not be reachable by UDM")
	}

	secretsSvc, err := secrets.New()
	if err != nil {
		log.Error("secrets init failed (set UNIFIX_SECRETS_KEY; use cmd/genkey to generate one)",
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
	// Der Master-Key (UNIFIX_SECRETS_KEY) verschluesselt den Pepper
	// im platform_config-Eintrag.
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

	mockMgr := mockmanager.New(database, log, mockmanager.Options{
		StateDirBase: cfg.MockStateDir,
		ServerIPv4:   cfg.ServerIPv4,
	})

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := mockMgr.LoadFromDB(ctx); err != nil {
		log.Error("mock manager load failed", "err", err)
		os.Exit(1)
	}
	defer func() {
		shutCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = mockMgr.Shutdown(shutCtx)
	}()

	// Saison 13-03: zentrale Push-Quelle plus Anruf-Lifecycle.
	// EventBus wird vom HTTPServer und vom doorbellhub geteilt,
	// damit ESP-SSE und Web-SSE aus derselben Quelle gefuettert
	// werden. doorbellcalls speichert pro Anruf den CAS-Stand
	// fuer Multi-Viewer-Annehmen.
	eventBus := eventbus.New()
	callsSvc := doorbellcalls.New(database.DB)
	hub := doorbellhub.NewWithOptions(mockMgr, historyStore, log, doorbellhub.Options{
		Bus:   eventBus,
		Calls: callsSvc,
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

	// access.UserStore-Adapter um den uaapi-Client. Nil-Client ist
	// erlaubt; der Adapter liefert dann access.ErrNotConfigured und
	// das Admin-UI zeigt einen Hinweis-Karten statt einer leeren
	// Liste.
	userStore := ua.New(uaClient)

	// Stream-Backend zeigt auf die StreamBackend-Naht
	// (Saison 15-01). Mit gesetzter CARVILON_STREAM_BACKEND_URL
	// wird der transitional go2rtc-Client konstruiert; ohne setzen
	// wir den 503-Default ein, sodass die Handler nie nil pruefen
	// muessen und alle Stream-Endpoints sauber degradieren. Die
	// commercial-Variante (carvilon-streaming-server) plugt spaeter
	// per Build-Tag dieselbe Naht; das oeffentliche Repo importiert
	// den privaten Server nicht.
	var streamBackend streams.StreamBackend = streams.Unconfigured()
	if cfg.StreamBackendURL != "" {
		c, err := streams.New(cfg.StreamBackendURL)
		if err != nil {
			log.Error("stream backend init failed", "err", err)
			os.Exit(1)
		}
		streamBackend = c
		// Boot-Log mit der vom Briefing geforderten Wortlaut, damit
		// der Operator nach jedem systemctl restart sofort sieht ob
		// /esp/stream.mjpeg und /webviewer/stream.mjpeg funktionieren.
		log.Info("stream backend configured", "url", cfg.StreamBackendURL)
	} else {
		log.Warn("stream backend not configured: /esp/stream.mjpeg, /webviewer/stream.mjpeg and /webviewer/offer return 503")
	}

	// Saison 14-01b: weather-Backend (open-meteo) ist immer aktiv;
	// braucht keinen API-Key. Bei Internet-Ausfall liefert der
	// Cache stale snapshots bis zu 24h; danach blendet die UI den
	// Wetter-Bereich aus.
	weatherClient := weather.New()
	log.Info("weather backend configured", "provider", "open-meteo")

	srv, err := httpserver.New(httpserver.Deps{
		Config:         cfg,
		Sessions:       sessionSvc,
		AdminSessions:  adminSessionSvc,
		MockManager:    mockMgr,
		Admin:          adminSvc,
		PlatformConfig: platformCfg,
		Audit:          auditSvc,
		ViewerLimiter:  viewerLimiter,
		AdminLimiter:   adminLimiter,
		UA:             uaClient,
		UserStore:      userStore,
		Hub:            hub,
		History:        historyStore,
		EventBus:       eventBus,
		DoorbellCalls:  callsSvc,
		Streams:        streamBackend,
		Weather:        weatherClient,
		Log:            log,
	})
	if err != nil {
		log.Error("httpserver init failed", "err", err)
		os.Exit(1)
	}

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

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server stopped", "err", err)
			os.Exit(1)
		}
	}
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

// startMDNSIfPossible parses listenAddr (":8080" or "0.0.0.0:8080")
// for the port and starts an mDNS advertisement. If serverIPv4 is
// empty (no UNIFIX_SERVER_IPV4 set), returns (nil, nil) - the
// caller logs a warning and continues without mDNS.
func startMDNSIfPossible(serverIPv4, listenAddr string, log *slog.Logger) (*mdns.Service, error) {
	if serverIPv4 == "" {
		return nil, errors.New("UNIFIX_SERVER_IPV4 not set")
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
