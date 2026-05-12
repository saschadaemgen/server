package main

import (
	"log"

	"unifix.local/server/internal/auth/magiclink"
	"unifix.local/server/internal/auth/session"
	"unifix.local/server/internal/config"
	"unifix.local/server/internal/db"
	"unifix.local/server/internal/httpserver"
)

func main() {
	cfg := config.FromEnv()
	if err := cfg.Validate(); err != nil {
		log.Fatalf("config invalid: %v", err)
	}

	database, err := db.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("db open failed: %v", err)
	}
	defer database.Close()

	magicSvc := magiclink.New(database)
	sessionSvc := session.New(database)

	srv := httpserver.New(cfg, magicSvc, sessionSvc)

	log.Printf("unifix-server starting on %s (devMode=%v, db=%s)",
		cfg.ListenAddr, cfg.DevMode, cfg.DBPath)

	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server stopped: %v", err)
	}
}
