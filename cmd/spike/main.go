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
	"carvilon.local/stream/internal/mjpeg"
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
	envFFmpegPath  = "CARVILON_FFMPEG"  // optional, defaults to "ffmpeg"
	envDisableMJPEG = "CARVILON_DISABLE_MJPEG"
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
	ffmpegPath := os.Getenv(envFFmpegPath) // empty = "ffmpeg" via $PATH
	if addr == "" {
		addr = defaultListen
	}
	if quality == "" {
		quality = defaultQuality
	}
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}

	if nvrHost == "" || apiKey == "" || cameraID == "" {
		logger.Fatalf("missing one of %s / %s / %s — see .env.example", envNVRHost, envAPIKey, envCameraID)
	}

	// Decide whether MJPEG is available. By default we enable it and
	// fail-fast if ffmpeg is missing — go2rtc-replacement is the whole
	// point of the binary, so a silent disable would be misleading.
	// CARVILON_DISABLE_MJPEG=1 turns it off (e.g. WebRTC-only dev runs
	// without an ffmpeg install).
	var mjpegProfiles []mjpeg.Profile
	if os.Getenv(envDisableMJPEG) == "" {
		if err := mjpeg.CheckFFmpeg(ffmpegPath); err != nil {
			logger.Fatalf("mjpeg startup check failed: %v\nSet %s=1 to disable MJPEG output.", err, envDisableMJPEG)
		}
		mjpegProfiles = mjpeg.DefaultProfiles()
		logger.Printf("mjpeg: enabled with profiles %v (ffmpeg=%q)", profileNames(mjpegProfiles), ffmpegPath)
	} else {
		logger.Printf("mjpeg: disabled via %s; only /offer (WebRTC) is served", envDisableMJPEG)
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
		MJPEGProfiles: mjpegProfiles,
		FFmpegPath:    ffmpegPath,
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

func profileNames(ps []mjpeg.Profile) []string {
	names := make([]string, len(ps))
	for i, p := range ps {
		names[i] = p.Name
	}
	return names
}
