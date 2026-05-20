// Command spike is the S1 step-1 feasibility binary.
//
// It pulls the configured RTSPS source and serves a tiny HTML page plus a
// /offer signaling endpoint so a single browser tab can view the live
// H.264 feed over WebRTC.
//
// Usage:
//
//	export CARVILON_STREAM_RTSP_SOURCE='rtsps://192.168.1.1:7441/<id>?enableSrtp'
//	# optional, defaults to :8555
//	export CARVILON_STREAM_LISTEN=':8555'
//	go run ./cmd/spike
//
// Then open http://<host>:8555/ in a LAN browser and click "Connect".
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"carvilon.local/stream"
)

const (
	envRTSP    = "CARVILON_STREAM_RTSP_SOURCE"
	envListen  = "CARVILON_STREAM_LISTEN"
	defaultLis = ":8555"
)

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)

	rtspURL := os.Getenv(envRTSP)
	if rtspURL == "" {
		logger.Fatalf("missing env %s — see .env.example", envRTSP)
	}
	addr := os.Getenv(envListen)
	if addr == "" {
		addr = defaultLis
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	src, err := stream.NewSource(stream.SourceOptions{
		RTSPURL:               rtspURL,
		Logger:                logger,
		InsecureSkipTLSVerify: true, // UDM cert has no IP-SAN; spike-only
	})
	if err != nil {
		logger.Fatalf("source: %v", err)
	}
	if err := src.Start(ctx); err != nil {
		logger.Fatalf("source start: %v", err)
	}
	defer func() { _ = src.Close() }()

	srv, err := stream.NewServer(stream.ServerOptions{
		Source: src,
		Addr:   addr,
		Logger: logger,
	})
	if err != nil {
		logger.Fatalf("server: %v", err)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		logger.Printf("shutdown signal received")
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("server: %v", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Printf("server shutdown: %v", err)
	}
}
