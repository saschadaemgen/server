package stream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"carvilon.local/stream/internal/profile"
	"carvilon.local/stream/internal/source"
	"carvilon.local/stream/internal/sourcereg"
	"carvilon.local/stream/internal/stats"
)

// freshServer builds a Server with the minimum wiring needed for the
// stats integration tests. The SourceFactory always errors — these
// tests don't hit handleMJPEG / handleOffer, just /stream/stats.
func freshServer(t *testing.T, opts ServerOptions) *Server {
	t.Helper()
	reg, err := profile.NewRegistry(nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if opts.Profiles == nil {
		opts.Profiles = reg
	}
	if opts.SourceFactory == nil {
		opts.SourceFactory = func(sourcereg.Key) (source.VideoSource, error) {
			return nil, errors.New("no source factory in stats test")
		}
	}
	if opts.Addr == "" {
		opts.Addr = ":0"
	}
	if opts.Logger == nil {
		opts.Logger = log.New(io.Discard, "", 0)
	}
	srv, err := NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(srv.shutdownAll)
	return srv
}

func TestStats_EmptyWhenNoStatsRegistry(t *testing.T) {
	// A Server with Stats=nil must still serve /stream/stats and
	// return a valid (zero) snapshot. The admin UI polls
	// unconditionally; an unconfigured stats slot must NOT 500.
	srv := freshServer(t, ServerOptions{})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stream/stats", nil)
	srv.handleStats(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Result().Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var snap stats.Snapshot
	if err := json.Unmarshal(rr.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rr.Body.String())
	}
	if snap.Global.Clients != 0 {
		t.Errorf("Global.Clients = %d, want 0", snap.Global.Clients)
	}
	if snap.Profiles == nil {
		t.Errorf("Profiles is nil; want empty map (so the UI can iterate without a nil check)")
	}
	if snap.Clients == nil {
		t.Errorf("Clients is nil; want empty slice")
	}
	if snap.GeneratedAt == "" {
		t.Errorf("GeneratedAt empty")
	}
}

func TestStats_RejectsNonGET(t *testing.T) {
	srv := freshServer(t, ServerOptions{})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/stream/stats", nil)
	srv.handleStats(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

func TestStats_ShowsRegisteredClient(t *testing.T) {
	// Wire a real stats.Registry and manually register a client (the
	// path handleMJPEG would take). The /stream/stats response must
	// include it.
	statsReg := stats.New()
	c := statsReg.Register("mjpeg_bal", "mjpeg", "1.2.3.4:5000")
	c.RecordFrame(1234)
	c.RecordFrame(1234)

	srv := freshServer(t, ServerOptions{Stats: statsReg})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stream/stats", nil)
	srv.handleStats(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}

	var snap stats.Snapshot
	if err := json.Unmarshal(rr.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rr.Body.String())
	}
	if snap.Global.Clients != 1 {
		t.Fatalf("Global.Clients = %d, want 1", snap.Global.Clients)
	}
	if snap.Global.FramesSentTotal != 2 {
		t.Errorf("FramesSentTotal = %d, want 2", snap.Global.FramesSentTotal)
	}
	if snap.Global.BytesSentTotal != 2468 {
		t.Errorf("BytesSentTotal = %d, want 2468", snap.Global.BytesSentTotal)
	}
	if len(snap.Clients) != 1 {
		t.Fatalf("Clients len = %d, want 1", len(snap.Clients))
	}
	cs := snap.Clients[0]
	if cs.Profile != "mjpeg_bal" || cs.Codec != "mjpeg" || cs.RemoteAddr != "1.2.3.4:5000" {
		t.Errorf("client mismatch: %+v", cs)
	}
	if _, ok := snap.Profiles["mjpeg_bal"]; !ok {
		t.Errorf("Profiles missing mjpeg_bal: %+v", snap.Profiles)
	}
}

// TestStats_LoggerExitsOnContextCancel asserts the periodic logger
// goroutine respects ctx cancellation — important for clean shutdown.
func TestStats_LoggerExitsOnContextCancel(t *testing.T) {
	srv := freshServer(t, ServerOptions{
		Stats:            stats.New(),
		StatsLogInterval: 100 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		srv.runStatsLogger(ctx)
		close(done)
	}()
	// Let the ticker fire at least once.
	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// ok
	case <-time.After(time.Second):
		t.Fatal("runStatsLogger did not exit within 1s of ctx cancel")
	}
}

func TestStats_LoggerIsNoOpWhenIntervalZero(t *testing.T) {
	// Sanity: with StatsLogInterval=0 the goroutine returns immediately.
	srv := freshServer(t, ServerOptions{
		Stats:            stats.New(),
		StatsLogInterval: 0,
	})
	done := make(chan struct{})
	go func() {
		srv.runStatsLogger(context.Background())
		close(done)
	}()
	select {
	case <-done:
		// ok
	case <-time.After(time.Second):
		t.Fatal("runStatsLogger with interval=0 did not return immediately")
	}
}
