package stream

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"carvilon.local/stream/internal/source"
	"carvilon.local/stream/internal/sourcereg"
)

func edgeOpts(t *testing.T) EdgeSetupOptions {
	t.Helper()
	return EdgeSetupOptions{
		NVRHost: "192.0.2.1", // TEST-NET-1, never dialed (factory is lazy)
		APIKey:  "dummy-key",
		DBPath:  filepath.Join(t.TempDir(), "stream.db"),
		Addr:    ":0",
		BaseURL: "http://127.0.0.1:8555",
		Logger:  log.New(io.Discard, "", 0),
	}
}

func TestSetupEdgeInProcess_Happy(t *testing.T) {
	srv, backend, shutdown, err := SetupEdgeInProcess(edgeOpts(t))
	if err != nil {
		t.Fatalf("SetupEdgeInProcess: %v", err)
	}
	if srv == nil {
		t.Fatal("srv is nil")
	}
	if backend == nil {
		t.Fatal("backend is nil")
	}
	if shutdown == nil {
		t.Fatal("shutdown is nil")
	}
	if err := shutdown(); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

// TestSetupEdgeInProcess_SharedRegistry proves the seam: a TrackForStream
// subscriber opened on the *Server is visible through the *Backend's
// consumer count for the same profile — they share ONE hub, hence one
// camera pull.
func TestSetupEdgeInProcess_SharedRegistry(t *testing.T) {
	// Use the in-package test seam: a fake source factory so TrackForStream
	// can subscribe instantly without dialing a real UDM. This isolates
	// the seam-sharing behaviour from any network.
	opts := edgeOpts(t)
	opts.sourceFactory = func(sourcereg.Key) (source.VideoSource, error) {
		return &fakeSource{frames: make(chan source.AccessUnit, 4)}, nil
	}
	srv, backend, shutdown, err := SetupEdgeInProcess(opts)
	if err != nil {
		t.Fatalf("SetupEdgeInProcess: %v", err)
	}
	t.Cleanup(func() { _ = shutdown() })

	ctx := context.Background()
	p := passthroughProfile("intercom_web", "cam-1")
	if err := backend.PutProfile(ctx, p); err != nil {
		t.Fatalf("PutProfile: %v", err)
	}

	// Backend sees zero consumers before anyone subscribes.
	if got, _ := backend.Get(ctx, "intercom_web"); got.Consumers != 0 {
		t.Fatalf("initial consumers = %d, want 0", got.Consumers)
	}

	// Open a server-side track (subscribes on the shared hub).
	_, stop, err := srv.TrackForStream("intercom_web")
	if err != nil {
		t.Fatalf("TrackForStream: %v", err)
	}
	defer stop()

	// The backend's consumer count for the SAME profile must now reflect
	// that subscriber — proof both facades share one registry/hub.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := backend.Get(ctx, "intercom_web"); got.Consumers == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, _ := backend.Get(ctx, "intercom_web")
	t.Fatalf("backend consumers = %d, want 1 (shared registry across the seam)", got.Consumers)
}

func TestSetupEdgeInProcess_ShutdownIdempotent(t *testing.T) {
	_, _, shutdown, err := SetupEdgeInProcess(edgeOpts(t))
	if err != nil {
		t.Fatalf("SetupEdgeInProcess: %v", err)
	}
	if err := shutdown(); err != nil {
		t.Errorf("first shutdown: %v", err)
	}
	if err := shutdown(); err != nil { // must not panic or error
		t.Errorf("second shutdown: %v", err)
	}
}

func TestSetupEdgeInProcess_MissingRequired(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*EdgeSetupOptions)
	}{
		{"no NVRHost", func(o *EdgeSetupOptions) { o.NVRHost = "" }},
		{"no APIKey", func(o *EdgeSetupOptions) { o.APIKey = "" }},
		{"no DBPath", func(o *EdgeSetupOptions) { o.DBPath = "" }},
		{"no Addr", func(o *EdgeSetupOptions) { o.Addr = "" }},
		{"no BaseURL", func(o *EdgeSetupOptions) { o.BaseURL = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := edgeOpts(t)
			tc.mutate(&opts)
			if _, _, _, err := SetupEdgeInProcess(opts); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}
