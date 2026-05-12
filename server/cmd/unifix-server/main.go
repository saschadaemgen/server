package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"unifix.local/server/internal/auth/admin"
	"unifix.local/server/internal/auth/adminsession"
	"unifix.local/server/internal/auth/magiclink"
	"unifix.local/server/internal/auth/session"
	"unifix.local/server/internal/config"
	"unifix.local/server/internal/db"
	"unifix.local/server/internal/doorbellhub"
	"unifix.local/server/internal/doorhistory"
	"unifix.local/server/internal/httpserver"
	"unifix.local/server/internal/mockmanager"
	"unifix.local/server/internal/platformconfig"
	"unifix.local/server/internal/secrets"
	"unifix.local/server/internal/uaapi"
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

	magicSvc := magiclink.New(database)
	sessionSvc := session.New(database)
	adminSessionSvc := adminsession.New(database)
	adminSvc := admin.New(database)
	platformCfg := platformconfig.New(database, secretsSvc)
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

	hub := doorbellhub.New(mockMgr, historyStore, log)
	go func() {
		if err := hub.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("doorbell hub stopped", "err", err)
		}
	}()

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

	srv, err := httpserver.New(httpserver.Deps{
		Config:         cfg,
		MagicLink:      magicSvc,
		Sessions:       sessionSvc,
		AdminSessions:  adminSessionSvc,
		MockManager:    mockMgr,
		Admin:          adminSvc,
		PlatformConfig: platformCfg,
		UA:             uaClient,
		Hub:            hub,
		History:        historyStore,
		Log:            log,
	})
	if err != nil {
		log.Error("httpserver init failed", "err", err)
		os.Exit(1)
	}

	log.Info("unifix-server starting",
		"addr", cfg.ListenAddr,
		"dev_mode", cfg.DevMode,
		"db", cfg.DBPath,
		"mock_state_dir", cfg.MockStateDir,
		"server_ipv4", cfg.ServerIPv4,
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
