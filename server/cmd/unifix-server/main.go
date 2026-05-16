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

	"unifix.local/server/internal/access/ua"
	"unifix.local/server/internal/auth/admin"
	"unifix.local/server/internal/auth/adminsession"
	"unifix.local/server/internal/auth/loginaudit"
	"unifix.local/server/internal/auth/ratelimit"
	"unifix.local/server/internal/auth/session"
	"unifix.local/server/internal/config"
	"unifix.local/server/internal/db"
	"unifix.local/server/internal/doorbellcalls"
	"unifix.local/server/internal/doorbellhub"
	"unifix.local/server/internal/doorhistory"
	"unifix.local/server/internal/eventbus"
	"unifix.local/server/internal/httpserver"
	"unifix.local/server/internal/mdns"
	"unifix.local/server/internal/mockmanager"
	"unifix.local/server/internal/platformconfig"
	"unifix.local/server/internal/secrets"
	"unifix.local/server/internal/streams"
	"unifix.local/server/internal/uaapi"
	"unifix.local/server/internal/weather"
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

	// Streams-Client zeigt auf das go2rtc-REST-API. Saison 14-01
	// schaltet UNIFIX_STREAM_BACKEND_URL produktiv; ohne diese Var
	// bleibt der Client nil und die /a/streams-Seite rendert den
	// "go2rtc nicht konfiguriert"-Hinweis, ohne den Server am Start
	// zu hindern.
	var streamsClient *streams.Client
	if cfg.StreamBackendURL != "" {
		c, err := streams.New(cfg.StreamBackendURL)
		if err != nil {
			log.Error("streams client init failed", "err", err)
			os.Exit(1)
		}
		streamsClient = c
		log.Info("streams backend configured", "url", cfg.StreamBackendURL)
	} else {
		log.Warn("UNIFIX_STREAM_BACKEND_URL not set; /esp/stream.mjpeg and /einloggen/stream.mjpeg will return 503")
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
		Streams:        streamsClient,
		Weather:        weatherClient,
		Log:            log,
	})
	if err != nil {
		log.Error("httpserver init failed", "err", err)
		os.Exit(1)
	}

	// mDNS-Advertisement (Saison 13-02-FIX4-d). Adoptierte
	// ESP-Viewer finden den Server via _unifix._tcp.local statt
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

	log.Info("unifix-server starting",
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
