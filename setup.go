package stream

import (
	"errors"
	"fmt"
	"log"
	"sync"

	"carvilon.local/stream/internal/proccpu"
	"carvilon.local/stream/internal/profile"
	"carvilon.local/stream/internal/source"
	"carvilon.local/stream/internal/source/unifi"
	"carvilon.local/stream/internal/sourcereg"
	"carvilon.local/stream/internal/stats"
	"carvilon.local/stream/internal/store"
	"carvilon.local/stream/internal/unifiapi"
	"carvilon.local/stream/streambackend"
)

// EdgeSetupOptions carries everything needed to construct the in-process
// streaming stack (Server + Backend sharing one source registry) for
// embedding in carvilon-edge.
type EdgeSetupOptions struct {
	// NVRHost is the UDM/Protect host (e.g. "192.168.1.1"). Required.
	NVRHost string
	// APIKey is the Protect integration X-API-KEY. Required.
	APIKey string
	// DBPath is the SQLite path (profiles persistence). Required.
	DBPath string
	// Encryption is the global camera-side wire mode ("tls"/"srtp"/"").
	// Empty -> tls.
	Encryption profile.Encryption
	// Addr is the HTTP listen address for the local stream server
	// (e.g. ":8555"). Required (the local MJPEG/Offer path stays HTTP).
	Addr string
	// BaseURL is the absolute root URL for MJPEG/Offer URL building
	// (e.g. "http://127.0.0.1:8555"). Required.
	BaseURL string
	// Logger - if nil, a default logger.
	Logger *log.Logger
	// FFmpegPath - defaults to "ffmpeg".
	FFmpegPath string
	// EnableMJPEG - whether the MJPEG/h264_cbp ffmpeg paths are active.
	EnableMJPEG bool
	// SubscriberBuffer - 0 means hub default.
	SubscriberBuffer int
	// Stats, CPU - optional, may be nil.
	Stats *stats.Registry
	CPU   *proccpu.Sampler

	// sourceFactory is an UNEXPORTED test seam: when set, it replaces the
	// built-in unifi source factory so in-package tests can subscribe
	// against a fake source without dialing a real UDM. Invisible to
	// external importers (carvilon-edge); production always uses the
	// unifi factory built from NVRHost/APIKey below.
	sourceFactory sourcereg.Factory
}

// SetupEdgeInProcess builds the in-process streaming stack: one shared
// source registry + profile registry feed BOTH a *Server (for
// TrackForStream / the local WebRTC+MJPEG HTTP path) and a
// *streambackend.Backend (for the carvilon StreamBackend surface:
// profile CRUD, MJPEG/Offer URLs, consumer counts).
//
// Because both share one [sourcereg.Registry], a local viewer and a
// remote (cloud-push) viewer of the same stream share a SINGLE camera
// pull. This is the whole reason the registry is built here and handed
// to both, instead of letting NewServer build its own (which would
// double-pull). See [ServerOptions.Sources].
//
// No seed happens here: the registry starts empty and the Backend
// hydrates it from whatever the store already holds. The built-in
// measurement default-set lives only in cmd/streaming-server, not in
// the embedded path (the carvilon admin fills the DB via CRUD).
//
// The returned shutdown closes the shared registry once (which owns the
// hubs and the source lifecycle) and the store. It is idempotent.
// Server and Backend are thin facades over the shared state.
func SetupEdgeInProcess(opts EdgeSetupOptions) (*Server, *streambackend.Backend, func() error, error) {
	switch {
	case opts.NVRHost == "":
		return nil, nil, nil, errors.New("stream: NVRHost is required")
	case opts.APIKey == "":
		return nil, nil, nil, errors.New("stream: APIKey is required")
	case opts.DBPath == "":
		return nil, nil, nil, errors.New("stream: DBPath is required")
	case opts.Addr == "":
		return nil, nil, nil, errors.New("stream: Addr is required")
	case opts.BaseURL == "":
		return nil, nil, nil, errors.New("stream: BaseURL is required")
	}

	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}

	st, err := store.Open(opts.DBPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("stream: store: %w", err)
	}

	// Empty registry; Backend.New hydrates from the store. No seed here.
	reg, err := profile.NewRegistry(nil)
	if err != nil {
		_ = st.Close()
		return nil, nil, nil, fmt.Errorf("stream: profile registry: %w", err)
	}

	// Source factory identical to cmd/streaming-server: the global
	// encryption mode reaches it via key.Encryption (stamped by the
	// server / backend from their canonical Encryption), so the factory
	// just trusts the key.
	nvrHost, apiKey := opts.NVRHost, opts.APIKey
	srcFactory := opts.sourceFactory
	if srcFactory == nil {
		srcFactory = func(key sourcereg.Key) (source.VideoSource, error) {
			return unifi.NewSource(unifi.Options{
				NVRHost:    nvrHost,
				APIKey:     apiKey,
				CameraID:   key.CameraID,
				Quality:    key.Quality,
				Encryption: unifi.Encryption(key.Encryption),
				Logger:     logger,
			})
		}
	}

	// THE shared registry — handed to both Server and Backend below.
	srcReg := sourcereg.New(srcFactory, logger)

	// Cameras client is optional: a construction failure (or a later
	// ListCameras failure) must not sink the whole stack — the Backend
	// tolerates a nil Cameras (ListCameras returns empty).
	cams, err := unifiapi.New(unifiapi.Options{NVRHost: opts.NVRHost, APIKey: opts.APIKey})
	if err != nil {
		logger.Printf("stream: unifiapi disabled (%v); ListCameras will be empty", err)
		cams = nil
	}

	backend, err := streambackend.New(streambackend.Options{
		Store:      st,
		Profiles:   reg,
		Sources:    srcReg,
		Cameras:    cams,
		BaseURL:    opts.BaseURL,
		Encryption: opts.Encryption,
	})
	if err != nil {
		_ = srcReg.Close()
		_ = st.Close()
		return nil, nil, nil, fmt.Errorf("stream: backend: %w", err)
	}

	srv, err := NewServer(ServerOptions{
		Profiles:         reg,    // SAME profile registry as the backend
		Sources:          srcReg, // SAME source registry -> one camera pull
		Addr:             opts.Addr,
		Logger:           logger,
		Encryption:       opts.Encryption,
		FFmpegPath:       opts.FFmpegPath,
		EnableMJPEG:      opts.EnableMJPEG,
		SubscriberBuffer: opts.SubscriberBuffer,
		Stats:            opts.Stats,
		CPU:              opts.CPU,
		ProfileWriter:    backend, // PUT/DELETE /api/profiles via the backend
	})
	if err != nil {
		_ = srcReg.Close()
		_ = st.Close()
		return nil, nil, nil, fmt.Errorf("stream: server: %w", err)
	}

	var once sync.Once
	shutdown := func() error {
		once.Do(func() {
			// Registry owns the hubs + source lifecycle; close it first,
			// then the store. sourcereg.Close is idempotent, so the
			// Server's own Run-shutdown closing it again is harmless.
			_ = srcReg.Close()
			_ = st.Close()
		})
		return nil
	}

	return srv, backend, shutdown, nil
}
