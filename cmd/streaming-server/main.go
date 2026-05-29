// Command streaming-server is the CARVILON video stream server.
//
// It runs in one of two roles selected by the -role flag (stdlib
// flag, no Cobra — see runEdge / runCloud):
//
//   - edge (default): the Season-1 logic. Hosts a small WebRTC
//     signaling endpoint and a go2rtc-compatible MJPEG endpoint, both
//     /offer and /api/stream.mjpeg keyed by ?src= profile name.
//     Profiles bind a camera, a Protect quality tier, and a usage
//     (browser / esp). Cameras are pulled only when watched (S4: 0
//     viewers = 0 RTSP pull, 0 ffmpeg, 0 decode).
//   - cloud: the Season-2 cloud-layer scaffold (WHIP ingress, fan-out,
//     WHEP egress). Stub today; real logic lands in S2-03 ff.
//
// As of S5, profiles persist in a SQLite database. As of S6-03, the
// spike comes with a built-in measurement-set default so the
// "100 measurement runs" workflow doesn't require any env-var typing.
// The startup chain is:
//
//  1. open the DB (create parent dir + schema if needed)
//  2. if the DB is empty, seed it. Priority order:
//     a. CARVILON_PROFILES_JSON  — explicit multi-camera config wins
//     b. UNIFI_CAMERA_ID         — S6-set on this camera
//     c. <none>                  — S6-set on the built-in default
//                                  intercom camera (see
//                                  defaultIntercomCameraID below)
//  3. load the in-memory registry from the DB
//  4. run.
//
// After the first start the DB is the source of truth; subsequent env
// changes are ignored (and the spike logs that fact explicitly).
//
// Zero-config quick-start (uses the built-in intercom camera):
//
//	$env:UNIFI_NVR_HOST = '192.168.1.1'
//	$env:UNIFI_API_KEY  = '<protect-integration-key>'
//	go run .\cmd\streaming-server
//
// — seeds five profiles on the intercom: intercom_web (browser/WebRTC),
// mjpeg_hq, mjpeg_bal, mjpeg_fast, h264_cbp.
//
// Single-camera quick-start (same set on a different camera):
//
//	$env:UNIFI_CAMERA_ID = '<other-camera-id>'
//	go run .\cmd\streaming-server
//
// Multi-camera quick-start (CARVILON_PROFILES_JSON overrides everything):
//
//	$env:CARVILON_PROFILES_JSON = '[
//	  {"name":"intercom_browser","camera_id":"abc","quality":"high","usage":"browser","codec":"h264_passthrough","description":"Intercom"},
//	  {"name":"intercom_mjpeg",  "camera_id":"abc","quality":"high","usage":"esp","codec":"mjpeg","width":800,"height":1280,"fps":12,"encode_quality":6},
//	  {"name":"ai360_browser",   "camera_id":"def","quality":"high","usage":"browser","codec":"h264_passthrough","description":"AI 360"}
//	]'
//	go run .\cmd\streaming-server
//
// The built-in default-set lives in cmd/streaming-server only — the carvilon-side
// production deployment (which links through streambackend.Backend)
// starts with an empty registry and lets the admin fill it via CRUD.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"carvilon.local/stream"
	"carvilon.local/stream/internal/mjpeg"
	"carvilon.local/stream/internal/proccpu"
	"carvilon.local/stream/internal/profile"
	"carvilon.local/stream/internal/source"
	"carvilon.local/stream/internal/source/unifi"
	"carvilon.local/stream/internal/sourcereg"
	"carvilon.local/stream/internal/stats"
	"carvilon.local/stream/internal/store"
	"carvilon.local/stream/internal/streamhub"
	"carvilon.local/stream/internal/unifiapi"
	"carvilon.local/stream/internal/whip"
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

	// defaultIntercomCameraID is the Intercom-on-the-test-UDM Protect
	// identifier. Used by the built-in S6 measurement default-set so
	// `go run .\cmd\streaming-server` with neither CARVILON_PROFILES_JSON nor
	// UNIFI_CAMERA_ID still produces a working set of profiles
	// pointing at the obvious camera. NOT a secret — the ID is a
	// device-local handle, the API key (which is the secret) is still
	// required via UNIFI_API_KEY. Hard-coding it here is deliberate
	// spike convenience; the carvilon-side production deployment
	// configures cameras via the admin UI, not through this fallback.
	defaultIntercomCameraID = "679573e101080b03e4000424"

	// --- cloud role (S2-03) ------------------------------------------------
	//
	// WHIP ingress listener. Cert/Key paths are installation-specific
	// (Sascha-convention: VPS user-homedir, not standardisable), so
	// they have NO default — runCloud Fatalf's when unset. On the VPS,
	// systemd's EnvironmentFile supplies the absolute paths.
	envWHIPListen          = "CARVILON_WHIP_LISTEN"            // optional, defaults to ":8444"
	envWHIPCertFile        = "CARVILON_WHIP_CERT_FILE"         // required in cloud mode
	envWHIPKeyFile         = "CARVILON_WHIP_KEY_FILE"          // required in cloud mode
	envPublishTokenHMACKey = "CARVILON_PUBLISH_TOKEN_HMAC_KEY" // required in cloud mode, 32-byte hex

	defaultWHIPListen = ":8444"
)

func main() {
	// S2-02: one binary, two roles via stdlib flag (no Cobra —
	// dependency doctrine + consistency with carvilon's S17-01 -role
	// choice). Default edge = the unchanged Season-1 path.
	var role string
	flag.StringVar(&role, "role", "edge", "Server-Rolle: edge|cloud")
	flag.Parse()

	switch role {
	case "edge":
		runEdge()
	case "cloud":
		runCloud()
	default:
		log.Fatalf("ungueltige -role: %q (erwarte edge|cloud)", role)
	}
}

// runCloud is the Season-2 cloud-role entry point. As of S2-03 it
// starts a real WHIP-ingress TLS listener with Bearer-token
// verification; on a verified token the endpoint still answers 501
// (track acceptance is S2-04). Required env: cert + key file paths and
// the 32-byte hex publish-token HMAC key. Missing config is fatal.
func runCloud() {
	logger := log.New(os.Stdout, "[cloud] ", log.LstdFlags|log.Lmicroseconds)

	addr := os.Getenv(envWHIPListen)
	if addr == "" {
		addr = defaultWHIPListen
	}
	certFile := os.Getenv(envWHIPCertFile)
	keyFile := os.Getenv(envWHIPKeyFile)
	hmacHex := os.Getenv(envPublishTokenHMACKey)

	if certFile == "" || keyFile == "" || hmacHex == "" {
		logger.Fatalf("missing one of %s, %s, %s — required in cloud mode",
			envWHIPCertFile, envWHIPKeyFile, envPublishTokenHMACKey)
	}

	hmacKey, err := hex.DecodeString(hmacHex)
	if err != nil {
		logger.Fatalf("%s: not valid hex: %v", envPublishTokenHMACKey, err)
	}
	if len(hmacKey) != 32 {
		logger.Fatalf("%s: must be 32 bytes hex-encoded (got %d bytes)", envPublishTokenHMACKey, len(hmacKey))
	}

	hub := streamhub.NewHub()

	srv, err := whip.New(whip.Config{
		Addr:     addr,
		CertFile: certFile,
		KeyFile:  keyFile,
		HMACKey:  hmacKey,
		Hub:      hub,
		Logger:   logger,
	})
	if err != nil {
		logger.Fatalf("whip server init: %v", err)
	}

	logger.Printf("WHIP-Ingress auf %s (cert=%s)", addr, certFile)
	logger.Printf("TODO S2-05: WHEP-Egress zum Hub")
	logger.Printf("TODO S2-06: RequestPublish-Call zu carvilon-cloud sidechannel.Server")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := srv.ListenAndServe(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatalf("whip server: %v", err)
	}
}

// runEdge is the Season-1 edge-role entry point — the original main()
// body, verbatim. It is the default when -role is omitted; behaviour
// is identical to the pre-S2 binary (open DB, seed, build registry +
// sources, serve HTTP on :8555).
func runEdge() {
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
	//
	// S6-14: the encryption mode is GLOBAL — populated into
	// stream.ServerOptions.Encryption and streambackend.Options.Encryption
	// from the UNIFI_ENCRYPTION env var (see below). The server then
	// stamps that value into every sourcereg.Key it builds, so the
	// factory just reads key.Encryption and trusts it. No env-fallback
	// here, no per-profile reading: this is the simplification that
	// fixed the S6-12 bug where env=srtp was silently ignored because
	// the per-profile encryption value canonicalised to "tls" and won.
	srcFactory := func(key sourcereg.Key) (source.VideoSource, error) {
		return unifi.NewSource(unifi.Options{
			NVRHost:    nvrHost,
			APIKey:     apiKey,
			CameraID:   key.CameraID,
			Quality:    key.Quality,
			Encryption: unifi.Encryption(key.Encryption),
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
		Store:      st,
		Profiles:   reg,
		Sources:    srcReg,
		Cameras:    cams,
		BaseURL:    baseURL,
		// S6-14: global encryption mode. Must match the value passed
		// to stream.NewServer below so both layers build the same
		// sourcereg.Key for the same camera.
		Encryption: profile.Encryption(encryption),
	})
	if err != nil {
		logger.Fatalf("backend: %v", err)
	}
	logger.Printf("backend: %d profile(s) loaded from DB: %v", len(reg.Names()), reg.Names())
	// `backend` is also used below as the ProfileWriter for the S6
	// tuning endpoints; the build-tagged carvilon wrapper consumes the
	// same pointer.

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

	// --- S6 Stats + CPU sampler --------------------------------------------
	//
	// Stats counts frames/bytes per client at the HTTP write boundary —
	// the wire-level half of the ESP measurement campaign. proccpu adds
	// the transcoder cost (Linux only; stub returns 0 on Windows/macOS
	// so dev builds compile and the JSON simply omits the field).
	statsReg := stats.New()
	cpuSampler, err := proccpu.NewSampler()
	if err != nil {
		logger.Printf("proccpu: disabled (%v); /stream/stats will omit CPU", err)
		cpuSampler = nil
	}

	// --- HTTP signaling server ---------------------------------------------
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := stream.NewServer(stream.ServerOptions{
		Profiles:         reg,
		SourceFactory:    srcFactory,
		Addr:             addr,
		Logger:           logger,
		FFmpegPath:       ffmpegPath,
		EnableMJPEG:      enableMJPEG,
		Stats:            statsReg,
		CPU:              cpuSampler,
		StatsLogInterval: 30 * time.Second,
		ProfileWriter:    backend, // S6-01 tuning: PUT/DELETE /api/profiles/{name}
		// S6-14: global camera-side wire-protection mode. Must match
		// the value passed to streambackend.New above so both layers
		// see the same value in sourceKeyFor.
		Encryption: profile.Encryption(encryption),
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
		return "S6 default-set via " + envCameraID
	}
	return "built-in S6 default-set (intercom camera)"
}

// loadSeedProfiles returns the profile list to seed the DB with if (and
// only if) the DB is currently empty. Preference order (S6-03):
//
//  1. CARVILON_PROFILES_JSON: the explicit multi-camera config wins
//     over everything — admin spelled it out, we obey.
//  2. UNIFI_CAMERA_ID: build the S6 measurement set on THIS camera.
//     Useful when the operator runs the spike against a non-Intercom
//     camera and still wants the standard measurement profiles.
//  3. Neither set: build the S6 measurement set on the hard-coded
//     [defaultIntercomCameraID] — pure spike convenience for the
//     "100 measurement runs" workflow where typing the JSON every time
//     was getting in the way. NOT used by the carvilon-side production
//     deployment: the streambackend Naht stays empty-start (the
//     carvilon admin fills the DB via CRUD); the default-set lives
//     exclusively in cmd/streaming-server.
//
// The streambackend never imports this file or the function — the
// boundary is enforced by package structure, not by a flag.
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
		cam = defaultIntercomCameraID
	}
	return defaultMeasurementProfileSet(cam), nil
}

// defaultMeasurementProfileSet returns the five S6 measurement profiles
// the spike seeds when no explicit JSON is given. The parameters are
// the briefing-pinned values; tweak via the CRUD endpoints
// (PUT /api/profiles/{name}) once the binary is up — no code change
// needed for further measurement runs.
//
// Profile cheat-sheet (S6-03 briefing):
//
//	intercom_web  browser  h264_passthrough  (WebRTC reference)
//	mjpeg_hq      esp      mjpeg   800x1280  10 fps  q=4
//	mjpeg_bal     esp      mjpeg   800x1280  12 fps  q=6
//	mjpeg_fast    esp      mjpeg   640x1024  18 fps  q=6
//	h264_cbp      esp      h264_cbp 800x1280 15 fps  CRF 26
//
// The h264_cbp profile only becomes addressable when S6-02 ships the
// /stream/h264 endpoint — until then it sits in the DB as a placeholder
// so the measurement-vs-MJPEG comparison row is already there for the
// admin UI / docs.
func defaultMeasurementProfileSet(cameraID string) []profile.Profile {
	return []profile.Profile{
		// Browser reference: WebRTC-passthrough, no transcode. Lets the
		// operator open localhost:8555 and confirm the camera is alive
		// before fiddling with MJPEG / H.264 parameters.
		{
			Name:        "intercom_web",
			CameraID:    cameraID,
			Quality:     profile.QualityHigh,
			Usage:       profile.UsageBrowser,
			Description: "Intercom (browser reference, H.264 passthrough via WebRTC)",
			Codec:       profile.CodecH264Passthrough,
		},

		// MJPEG measurement triplet — covers the bandwidth / framerate
		// span the ESP32-P4 + intercom screen can plausibly handle.
		{
			Name:          "mjpeg_hq",
			CameraID:      cameraID,
			Quality:       profile.QualityHigh,
			Usage:         profile.UsageESP,
			Description:   "ESP: MJPEG, 800x1280 @ 10 fps, q:v 4 (high quality)",
			Codec:         profile.CodecMJPEG,
			Width:         800,
			Height:        1280,
			FPS:           10,
			EncodeQuality: 4,
		},
		{
			Name:          "mjpeg_bal",
			CameraID:      cameraID,
			Quality:       profile.QualityHigh,
			Usage:         profile.UsageESP,
			Description:   "ESP: MJPEG, 800x1280 @ 12 fps, q:v 6 (balanced)",
			Codec:         profile.CodecMJPEG,
			Width:         800,
			Height:        1280,
			FPS:           12,
			EncodeQuality: 6,
		},
		{
			Name:          "mjpeg_fast",
			CameraID:      cameraID,
			Quality:       profile.QualityHigh,
			Usage:         profile.UsageESP,
			Description:   "ESP: MJPEG, 640x1024 @ 18 fps, q:v 6 (fast)",
			Codec:         profile.CodecMJPEG,
			Width:         640,
			Height:        1024,
			FPS:           18,
			EncodeQuality: 6,
		},

		// H.264 Constrained Baseline — the S6-01/02 experiment. CRF 26
		// is the libx264 sweet-spot start for low-latency streaming;
		// tune via CRUD as the measurements come in. Only addressable
		// once /stream/h264 (S6-02) ships; sits in the DB beforehand so
		// the row is ready when the endpoint lands.
		{
			Name:          "h264_cbp",
			CameraID:      cameraID,
			Quality:       profile.QualityHigh,
			Usage:         profile.UsageESP,
			Description:   "ESP: H.264 Constrained Baseline, 800x1280 @ 15 fps, CRF 26",
			Codec:         profile.CodecH264CBP,
			Width:         800,
			Height:        1280,
			FPS:           15,
			EncodeQuality: 26,
		},
	}
}
