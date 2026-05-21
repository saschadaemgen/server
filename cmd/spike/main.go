// Command spike is the streaming-server feasibility binary.
//
// It hosts a small WebRTC signaling endpoint and a go2rtc-compatible
// MJPEG endpoint, both /offer and /api/stream.mjpeg keyed by ?src=
// profile name. Profiles bind a camera, a Protect quality tier, and a
// usage (browser / esp). Cameras are pulled only when watched
// (S4: 0 viewers = 0 RTSP pull, 0 ffmpeg, 0 decode).
//
// As of S5, profiles persist in a SQLite database. The startup chain
// is: open the DB → if empty, seed from CARVILON_PROFILES_JSON
// (or the S6 default-set built from UNIFI_CAMERA_ID) → load the
// in-memory registry from the DB → run. After the first start the DB
// is the source of truth; subsequent env changes to PROFILES_JSON are
// ignored (and the spike logs that fact explicitly).
//
// Single-camera quick-start (seeds five profiles on that camera: one
// browser/h264_passthrough plus the four ESP measurement profiles —
// mjpeg_hq, mjpeg_bal, mjpeg_fast, h264_cbp):
//
//	$env:UNIFI_NVR_HOST   = '192.168.1.1'
//	$env:UNIFI_API_KEY    = '<protect-integration-key>'
//	$env:UNIFI_CAMERA_ID  = '<camera-id>'
//	go run .\cmd\spike
//
// Multi-camera quick-start (only takes effect when the DB is empty;
// CARVILON_PROFILES_JSON overrides the default-set):
//
//	$env:CARVILON_PROFILES_JSON = '[
//	  {"name":"intercom_browser","cameraID":"abc","quality":"high","usage":"browser","codec":"h264_passthrough","description":"Intercom"},
//	  {"name":"intercom_mjpeg",  "cameraID":"abc","quality":"high","usage":"esp","codec":"mjpeg","width":800,"height":1280,"fps":12,"encodeQuality":6},
//	  {"name":"ai360_browser",   "cameraID":"def","quality":"high","usage":"browser","codec":"h264_passthrough","description":"AI 360"}
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
	"carvilon.local/stream/internal/store"
	"carvilon.local/stream/internal/unifiapi"
	"carvilon.local/stream/streambackend"
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
	envDBPath       = "CARVILON_STREAM_DB" // optional, defaults to ./state/stream.db
	envBaseURL      = "CARVILON_STREAM_BASE_URL"

	defaultListen = ":8555"
	defaultDBPath = "./state/stream.db"
)

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)

	nvrHost := os.Getenv(envNVRHost)
	apiKey := os.Getenv(envAPIKey)
	addr := os.Getenv(envListen)
	encryption := os.Getenv(envEncryption)
	ffmpegPath := os.Getenv(envFFmpegPath)
	dbPath := os.Getenv(envDBPath)
	baseURL := os.Getenv(envBaseURL)
	if addr == "" {
		addr = defaultListen
	}
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	if dbPath == "" {
		dbPath = defaultDBPath
	}
	if baseURL == "" {
		baseURL = "http://127.0.0.1" + addr
	}

	if nvrHost == "" || apiKey == "" {
		logger.Fatalf("missing %s or %s — see .env.example", envNVRHost, envAPIKey)
	}

	// --- S5 store + seed chain ---------------------------------------------
	//
	// Step 1: open the DB (creates parent dir + schema if needed).
	st, err := store.Open(dbPath)
	if err != nil {
		logger.Fatalf("store: %v", err)
	}
	defer func() { _ = st.Close() }()
	logger.Printf("store: opened %s", st.Path())

	// Step 2: if the DB is empty AND a seed source is set, seed it. The
	// DB then becomes the single source of truth — subsequent restarts
	// ignore PROFILES_JSON / UNIFI_CAMERA_ID (and log the fact).
	seed, err := loadSeedProfiles()
	if err != nil {
		logger.Fatalf("seed config: %v", err)
	}
	ctxSeed, cancelSeed := context.WithTimeout(context.Background(), 5*1000*1000*1000)
	inserted, err := st.SeedIfEmpty(ctxSeed, seed)
	cancelSeed()
	if err != nil {
		logger.Fatalf("seed: %v", err)
	}
	switch {
	case inserted > 0:
		logger.Printf("store: seeded %d profile(s) from %s into empty DB; DB is the source of truth from now on", inserted, seedSource())
	case len(seed) > 0:
		logger.Printf("store: ignoring %s (%d entries) — DB is already populated, DB wins", seedSource(), len(seed))
	default:
		logger.Printf("store: no seed configured")
	}

	// Step 3: build the in-memory registry from whatever's in the DB.
	reg, err := profile.NewRegistry(nil)
	if err != nil {
		logger.Fatalf("profile registry: %v", err)
	}

	// --- Source factory + Protect-API client -------------------------------
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
	srcReg := sourcereg.New(srcFactory, logger)
	defer func() { _ = srcReg.Close() }()

	cams, err := unifiapi.New(unifiapi.Options{
		NVRHost: nvrHost,
		APIKey:  apiKey,
	})
	if err != nil {
		logger.Fatalf("unifiapi: %v", err)
	}

	// --- StreamBackend (carvilon naht) -------------------------------------
	//
	// New() hydrates the in-memory registry from the DB. The HTTP server
	// (built below) shares the same registry, so it sees the same data.
	backend, err := streambackend.New(streambackend.Options{
		Store:    st,
		Profiles: reg,
		Sources:  srcReg,
		Cameras:  cams,
		BaseURL:  baseURL,
	})
	if err != nil {
		logger.Fatalf("backend: %v", err)
	}
	_ = backend // bound but unused locally; carvilon-side wrapper picks it up
	logger.Printf("backend: %d profile(s) loaded from DB: %v", len(reg.Names()), reg.Names())

	// --- MJPEG check --------------------------------------------------------
	enableMJPEG := os.Getenv(envDisableMJPEG) == ""
	if enableMJPEG {
		if err := mjpeg.CheckFFmpeg(ffmpegPath); err != nil {
			logger.Fatalf("mjpeg startup check failed: %v\nSet %s=1 to disable MJPEG output.", err, envDisableMJPEG)
		}
		logger.Printf("mjpeg: enabled (ffmpeg=%q)", ffmpegPath)
	} else {
		logger.Printf("mjpeg: disabled via %s; only /offer (WebRTC) is served", envDisableMJPEG)
	}

	// --- HTTP signaling server ---------------------------------------------
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

// seedSource returns a human-readable label for whichever seed source
// is active. Pure cosmetic, used only in log messages.
func seedSource() string {
	if os.Getenv(envProfilesJSON) != "" {
		return envProfilesJSON
	}
	if os.Getenv(envCameraID) != "" {
		return "single-camera defaults via " + envCameraID
	}
	return "<no seed>"
}

// loadSeedProfiles returns the profile list to seed the DB with if (and
// only if) the DB is currently empty. Preference order:
//
//  1. CARVILON_PROFILES_JSON: the explicit multi-camera config.
//  2. UNIFI_CAMERA_ID: builds the S6 default-set for that single
//     camera — one browser profile plus the four ESP measurement
//     profiles (mjpeg_hq, mjpeg_bal, mjpeg_fast, h264_cbp). The admin
//     can edit / delete any of them through the CRUD surface.
//  3. Empty: no profiles to seed. The DB stays empty until the admin
//     creates profiles through the CRUD surface.
//
// S6-01 rationale for the default-set: shipping four ESP profiles out
// of the box means the measurement campaign starts the moment the
// binary is up — no manual config required to compare mjpeg vs.
// h264_cbp on the same camera.
func loadSeedProfiles() ([]profile.Profile, error) {
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
	return []profile.Profile{
		// Browser fallback: WebRTC-passthrough, no transcode.
		{
			Name:        "intercom_browser",
			CameraID:    cam,
			Quality:     profile.QualityHigh,
			Usage:       profile.UsageBrowser,
			Description: "Intercom (browser, H.264 passthrough via WebRTC)",
			Codec:       profile.CodecH264Passthrough,
		},

		// MJPEG measurement triplet — covers the bandwidth / framerate
		// extremes the ESP can plausibly handle. The 'bal' profile
		// mirrors the S5 default so existing measurements stay
		// comparable.
		{
			Name:          "mjpeg_hq",
			CameraID:      cam,
			Quality:       profile.QualityHigh,
			Usage:         profile.UsageESP,
			Description:   "ESP: MJPEG, 800x1280 @ 9 fps, q:v 4 (high quality)",
			Codec:         profile.CodecMJPEG,
			Width:         800,
			Height:        1280,
			FPS:           9,
			EncodeQuality: 4,
		},
		{
			Name:          "mjpeg_bal",
			CameraID:      cam,
			Quality:       profile.QualityHigh,
			Usage:         profile.UsageESP,
			Description:   "ESP: MJPEG, 800x1280 @ 12 fps, q:v 6 (balanced — S5 default)",
			Codec:         profile.CodecMJPEG,
			Width:         800,
			Height:        1280,
			FPS:           12,
			EncodeQuality: 6,
		},
		{
			Name:          "mjpeg_fast",
			CameraID:      cam,
			Quality:       profile.QualityHigh,
			Usage:         profile.UsageESP,
			Description:   "ESP: MJPEG, 640x1024 @ 15 fps, q:v 8 (fast)",
			Codec:         profile.CodecMJPEG,
			Width:         640,
			Height:        1024,
			FPS:           15,
			EncodeQuality: 8,
		},

		// H.264 Constrained Baseline — the S6-01 experiment. ESP32-P4
		// + tinyH264 can decode this; we want to see whether the byte
		// budget per frame beats MJPEG at similar visual quality.
		// CRF 26 is the libx264 sweet-spot start for low-latency
		// streaming; tune via CRUD as the measurements come in.
		{
			Name:          "h264_cbp",
			CameraID:      cam,
			Quality:       profile.QualityHigh,
			Usage:         profile.UsageESP,
			Description:   "ESP: H.264 Constrained Baseline, 800x1280 @ 12 fps, CRF 26",
			Codec:         profile.CodecH264CBP,
			Width:         800,
			Height:        1280,
			FPS:           12,
			EncodeQuality: 26,
		},
	}, nil
}
