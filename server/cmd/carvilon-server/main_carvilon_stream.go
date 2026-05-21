//go:build carvilon_stream

// Saison 15: commercial main.go entry that wires the private
// streaming-server backend instead of the unconfigured default. The
// public main.go uses the default; this file replaces the wiring when
// built with -tags carvilon_stream.
//
// Saison-15-07-Nachtrag: import paths use the canonical
// carvilon.local/... module names (not the github.com/saschadaemgen
// shape that the INTEGRATION.md template carried).
package main

import (
	"log"

	privateProfile "carvilon.local/stream/internal/profile"
	privateSource "carvilon.local/stream/internal/source"
	privateUnifi "carvilon.local/stream/internal/source/unifi"
	privateSourcereg "carvilon.local/stream/internal/sourcereg"
	privateStore "carvilon.local/stream/internal/store"
	privateUnifiAPI "carvilon.local/stream/internal/unifiapi"
	private "carvilon.local/stream/streambackend"

	"carvilon.local/server/internal/streams"
)

// init replaces the default 503-backend slot in the public main.go.
func init() {
	st, err := privateStore.Open(streamDBPath())
	if err != nil {
		log.Fatalf("commercial: store: %v", err)
	}

	reg, err := privateProfile.NewRegistry(nil)
	if err != nil {
		log.Fatalf("commercial: profile registry: %v", err)
	}

	srcReg := privateSourcereg.New(
		func(k privateSourcereg.Key) (privateSource.VideoSource, error) {
			return privateUnifi.NewSource(privateUnifi.Options{
				NVRHost:  envOrFatal("UNIFI_NVR_HOST"),
				APIKey:   envOrFatal("UNIFI_API_KEY"),
				CameraID: k.CameraID,
				Quality:  k.Quality,
			})
		},
		log.Default(),
	)

	cams, err := privateUnifiAPI.New(privateUnifiAPI.Options{
		NVRHost: envOrFatal("UNIFI_NVR_HOST"),
		APIKey:  envOrFatal("UNIFI_API_KEY"),
	})
	if err != nil {
		log.Fatalf("commercial: unifiapi: %v", err)
	}

	backend, err := private.New(private.Options{
		Store:    st,
		Profiles: reg,
		Sources:  srcReg,
		Cameras:  cams,
		BaseURL:  envOrFatal("CARVILON_STREAM_BASE_URL"),
	})
	if err != nil {
		log.Fatalf("commercial: backend: %v", err)
	}

	commercialBackend = streams.NewCarvilonStreamBackend(backend)
}
