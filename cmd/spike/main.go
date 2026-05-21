// Command spike is the S1, Schritt 1 feasibility binary.
//
// It pulls the configured UniFi Protect camera via the integration API
// and the camera's RTSPS endpoint, runs the H.264 stream through the
// in-tree RFC-6184 depacketizer, and serves a tiny HTML page plus a
// /offer signaling endpoint so a single browser tab can view the live
// feed over WebRTC.
//
// Usage (PowerShell):
//
//	$env:UNIFI_NVR_HOST   = '192.168.1.1'
//	$env:UNIFI_API_KEY    = '<protect-integration-key>'
//	$env:UNIFI_CAMERA_ID  = '<camera-id>'
//	# optional, defaults to :8555
//	# $env:CARVILON_STREAM_LISTEN = ':8555'
//	go run .\cmd\spike
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

	"carvilon.local/stream"
	"carvilon.local/stream/internal/source/unifi"
)

const (
	envNVRHost      = "UNIFI_NVR_HOST"
	envAPIKey       = "UNIFI_API_KEY"
	envCameraID     = "UNIFI_CAMERA_ID"
	envListen       = "CARVILON_STREAM_LISTEN"
	envQuality      = "UNIFI_QUALITY" // optional, defaults to "high"
	defaultListen   = ":8555"
	defaultQuality  = "high"
)

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)

	nvrHost := os.Getenv(envNVRHost)
	apiKey := os.Getenv(envAPIKey)
	cameraID := os.Getenv(envCameraID)
	addr := os.Getenv(envListen)
	quality := os.Getenv(envQuality)
	if addr == "" {
		addr = defaultListen
	}
	if quality == "" {
		quality = defaultQuality
	}

	if nvrHost == "" || apiKey == "" || cameraID == "" {
		logger.Fatalf("missing one of %s / %s / %s — see .env.example", envNVRHost, envAPIKey, envCameraID)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	src, err := unifi.NewSource(unifi.Options{
		NVRHost:  nvrHost,
		APIKey:   apiKey,
		CameraID: cameraID,
		Quality:  quality,
		Logger:   logger,
	})
	if err != nil {
		logger.Fatalf("unifi source: %v", err)
	}
	if err := src.Start(ctx); err != nil {
		logger.Fatalf("unifi start: %v", err)
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

	if err := srv.Run(ctx); err != nil &&
		!errors.Is(err, context.Canceled) &&
		!errors.Is(err, http.ErrServerClosed) {
		logger.Printf("server: %v", err)
	}
}
