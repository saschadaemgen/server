// Command spike is the S1+S2+S3+S4 feasibility binary.
//
// It hosts a small WebRTC signaling endpoint and a go2rtc-compatible
// MJPEG endpoint, both /offer and /api/stream.mjpeg keyed by ?src=
// profile name. Profiles bind a camera, a Protect quality tier, and a
// usage (browser / esp). Cameras are pulled only when watched
// (S4-01: 0 viewers = 0 RTSP pull, 0 ffmpeg, 0 decode).
//
// Usage (PowerShell, single-camera backward-compatible):
//
//	$env:UNIFI_NVR_HOST   = '192.168.1.1'
//	$env:UNIFI_API_KEY    = '<protect-integration-key>'
//	$env:UNIFI_CAMERA_ID  = '<camera-id>'           # default profile builder
//	go run .\cmd\spike
//
// For multi-camera: set CARVILON_PROFILES_JSON to a JSON array, e.g.
//
//	$env:CARVILON_PROFILES_JSON = '[
//	  {"name":"intercom_browser","cameraID":"abc","quality":"high","usage":"browser","description":"Intercom"},
//	  {"name":"intercom_esp",    "cameraID":"abc","quality":"high","usage":"esp"},
//	  {"name":"ai360_browser",   "cameraID":"def","quality":"high","usage":"browser","description":"AI 360"}
//	]'
//	go run .\cmd\spike
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"carvilon.local/stream"
	"carvilon.local/stream/internal/mjpeg"
	"carvilon.local/stream/internal/profile"
	"carvilon.local/stream/internal/source"
	"carvilon.local/stream/internal/source/unifi"
	"carvilon.local/stream/internal/sourcereg"
)

const (
	envNVRHost      = "UNIFI_NVR_HOST"
	envAPIKey       = "UNIFI_API_KEY"
	envCameraID     = "UNIFI_CAMERA_ID"
	envListen       = "CARVILON_STREAM_LISTEN"
	envEncryption   = "UNIFI_ENCRYPTION" // optional, defaults to "tls"
	envFFmpegPath   = "CARVILON_FFMPEG"  // optional, defaults to "ffmpeg"
	envDisableMJPEG = "CARVILON_DISABLE_MJPEG"
	envProfilesJSON = "CARVILON_PROFILES_JSON"

	defaultListen = ":8555"
)

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)

	nvrHost := os.Getenv(envNVRHost)
	apiKey := os.Getenv(envAPIKey)
	addr := os.Getenv(envListen)
	encryption := os.Getenv(envEncryption)
	ffmpegPath := os.Getenv(envFFmpegPath)
	if addr == "" {
		addr = defaultListen
	}
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}

	if nvrHost == "" || apiKey == "" {
		logger.Fatalf("missing %s or %s — see .env.example", envNVRHost, envAPIKey)
	}

	// Profile configuration:
	//   1. CARVILON_PROFILES_JSON, if set, is the full list (multi-camera).
	//   2. Otherwise: UNIFI_CAMERA_ID drives a built-in default pair
	//      (browser + esp on that one camera). Backward-compat with
	//      the S1..S3 single-camera spike.
	profiles, err := loadProfiles()
	if err != nil {
		logger.Fatalf("profile config: %v", err)
	}
	if len(profiles) == 0 {
		logger.Fatalf("no profiles registered — set %s or %s", envProfilesJSON, envCameraID)
	}

	reg, err := profile.NewRegistry(profiles)
	if err != nil {
		logger.Fatalf("profile registry: %v", err)
	}
	logger.Printf("profiles: %v", reg.Names())

	// MJPEG availability check.
	enableMJPEG := os.Getenv(envDisableMJPEG) == ""
	if enableMJPEG {
		if err := mjpeg.CheckFFmpeg(ffmpegPath); err != nil {
			logger.Fatalf("mjpeg startup check failed: %v\nSet %s=1 to disable MJPEG output.", err, envDisableMJPEG)
		}
		logger.Printf("mjpeg: enabled (ffmpeg=%q)", ffmpegPath)
	} else {
		logger.Printf("mjpeg: disabled via %s; only /offer (WebRTC) is served", envDisableMJPEG)
	}

	// Source factory: builds a fresh unifi.Source for any (CameraID,
	// Quality) key. The source registry calls this lazily — never at
	// startup, only when a viewer for that key arrives.
	srcFactory := func(key sourcereg.Key) (source.VideoSource, error) {
		return unifi.NewSource(unifi.Options{
			NVRHost:    nvrHost,
			APIKey:     apiKey,
			CameraID:   key.CameraID,
			Quality:    key.Quality,
			Encryption: unifi.Encryption(encryption),
			Logger:     logger,
		})
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := stream.NewServer(stream.ServerOptions{
		Profiles:      reg,
		SourceFactory: srcFactory,
		Addr:          addr,
		Logger:        logger,
		FFmpegPath:    ffmpegPath,
		EnableMJPEG:   enableMJPEG,
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

// loadProfiles returns the configured profile list, preferring the
// JSON env var over the single-camera convenience defaults.
func loadProfiles() ([]profile.Profile, error) {
	if raw := os.Getenv(envProfilesJSON); raw != "" {
		var ps []profile.Profile
		if err := json.Unmarshal([]byte(raw), &ps); err != nil {
			return nil, fmt.Errorf("%s: %w", envProfilesJSON, err)
		}
		return ps, nil
	}
	cam := os.Getenv(envCameraID)
	if cam == "" {
		return nil, nil
	}
	// Backward-compat default: one camera, two profiles (browser + esp),
	// both on the "high" quality tier — mirrors the S3 default that the
	// existing carvilon-proxy expects.
	return []profile.Profile{
		{
			Name:        "intercom_browser",
			CameraID:    cam,
			Quality:     profile.QualityHigh,
			Usage:       profile.UsageBrowser,
			Description: "Intercom (browser)",
		},
		{
			Name:        "intercom_esp",
			CameraID:    cam,
			Quality:     profile.QualityHigh,
			Usage:       profile.UsageESP,
			Description: "Intercom (ESP)",
		},
	}, nil
}
