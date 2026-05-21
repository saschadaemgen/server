// Command spike is the S1+S2 feasibility binary.
//
// It hosts a tiny WebRTC signaling endpoint at the configured listen
// address and fans out a single UniFi Protect camera pull to any number
// of concurrent browser viewers (Fan-Out, see S2-01). The first
// connecting viewer triggers the camera bring-up; the last leaving viewer
// shuts it down again so the camera is idle when nobody is watching.
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
// Open it in several tabs / browsers to exercise fan-out — they should
// all see the same live feed without the camera being pulled more than
// once.
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
	"carvilon.local/stream/internal/source"
	"carvilon.local/stream/internal/source/unifi"
)

const (
	envNVRHost     = "UNIFI_NVR_HOST"
	envAPIKey      = "UNIFI_API_KEY"
	envCameraID    = "UNIFI_CAMERA_ID"
	envListen      = "CARVILON_STREAM_LISTEN"
	envQuality     = "UNIFI_QUALITY"    // optional, defaults to "high"
	envEncryption  = "UNIFI_ENCRYPTION" // optional, defaults to "tls"
	defaultListen  = ":8555"
	defaultQuality = "high"
)

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)

	nvrHost := os.Getenv(envNVRHost)
	apiKey := os.Getenv(envAPIKey)
	cameraID := os.Getenv(envCameraID)
	addr := os.Getenv(envListen)
	quality := os.Getenv(envQuality)
	encryption := os.Getenv(envEncryption) // empty = unifi default (tls)
	if addr == "" {
		addr = defaultListen
	}
	if quality == "" {
		quality = defaultQuality
	}

	if nvrHost == "" || apiKey == "" || cameraID == "" {
		logger.Fatalf("missing one of %s / %s / %s — see .env.example", envNVRHost, envAPIKey, envCameraID)
	}

	// Source factory: invoked lazily by the hub on first viewer and again
	// after every down-to-zero cycle, so each lifetime gets a fresh
	// gortsplib client (the previous one's channels are closed and not
	// reusable).
	srcFactory := func() (source.VideoSource, error) {
		return unifi.NewSource(unifi.Options{
			NVRHost:    nvrHost,
			APIKey:     apiKey,
			CameraID:   cameraID,
			Quality:    quality,
			Encryption: unifi.Encryption(encryption),
			Logger:     logger,
		})
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := stream.NewServer(stream.ServerOptions{
		SourceFactory: srcFactory,
		Addr:          addr,
		Logger:        logger,
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
