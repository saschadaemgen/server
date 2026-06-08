package stream

import (
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"carvilon.local/stream/internal/profile"
	"carvilon.local/stream/internal/source"
	"carvilon.local/stream/internal/sourcereg"
)

// --- fake source -------------------------------------------------------------

// fakeSource is a [source.VideoSource] with a hand-driven frames channel
// and a Start counter, mirroring internal/hub's test fake. It never
// emits frames on its own; the feedTrack idle watchdog (or an explicit
// stop) ends consumers.
type fakeSource struct {
	frames    chan source.AccessUnit
	starts    *atomic.Int64
	closeOnce sync.Once
}

func (f *fakeSource) Start(context.Context) error {
	if f.starts != nil {
		f.starts.Add(1)
	}
	return nil
}
func (f *fakeSource) Frames() <-chan source.AccessUnit { return f.frames }
func (f *fakeSource) Params() source.H264Params        { return source.H264Params{} }
func (f *fakeSource) Close() error {
	f.closeOnce.Do(func() { close(f.frames) })
	return nil
}

// trackTestServer builds a Server with a fake source factory (no real
// UDM) and the given profiles. starts counts every source Start across
// all camera keys — the shared-pull proof reads it.
func trackTestServer(t *testing.T, profiles []profile.Profile) (*Server, *atomic.Int64) {
	t.Helper()
	reg, err := profile.NewRegistry(profiles)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	var starts atomic.Int64
	factory := func(sourcereg.Key) (source.VideoSource, error) {
		return &fakeSource{frames: make(chan source.AccessUnit, 4), starts: &starts}, nil
	}
	srv, err := NewServer(ServerOptions{
		Profiles:      reg,
		SourceFactory: factory,
		Addr:          ":0",
		Logger:        log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(srv.shutdownAll)
	return srv, &starts
}

func passthroughProfile(name, cam string) profile.Profile {
	return profile.Profile{
		Name: name, CameraID: cam, Quality: profile.QualityHigh,
		Usage: profile.UsageBrowser, Codec: profile.CodecH264Passthrough,
	}
}

func mjpegProfile(name, cam string) profile.Profile {
	return profile.Profile{
		Name: name, CameraID: cam, Quality: profile.QualityHigh,
		Usage: profile.UsageESP, Codec: profile.CodecMJPEG,
		Width: 800, Height: 1280, FPS: 12, EncodeQuality: 6,
	}
}

func subscriberCount(s *Server, p profile.Profile) int {
	return s.sources.HubFor(s.sourceKeyFor(p)).SubscriberCount()
}

func waitForCount(t *testing.T, s *Server, p profile.Profile, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if subscriberCount(s, p) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("subscriber count = %d, want %d", subscriberCount(s, p), want)
}

// --- tests -------------------------------------------------------------------

func TestTrackForStream_UnknownProfile(t *testing.T) {
	srv, _ := trackTestServer(t, nil)
	_, _, err := srv.TrackForStream("nope")
	if !errors.Is(err, profile.ErrUnknownProfile) {
		t.Errorf("err = %v, want ErrUnknownProfile", err)
	}
}

func TestTrackForStream_WrongCodec(t *testing.T) {
	srv, _ := trackTestServer(t, []profile.Profile{mjpegProfile("mjpeg_bal", "cam-1")})
	_, _, err := srv.TrackForStream("mjpeg_bal")
	if err == nil {
		t.Fatal("expected error for non-passthrough codec")
	}
	if errors.Is(err, profile.ErrUnknownProfile) {
		t.Errorf("err = %v, want a codec-gate error, not ErrUnknownProfile", err)
	}
}

func TestTrackForStream_Happy(t *testing.T) {
	p := passthroughProfile("intercom_web", "cam-1")
	srv, _ := trackTestServer(t, []profile.Profile{p})

	track, stop, err := srv.TrackForStream("intercom_web")
	if err != nil {
		t.Fatalf("TrackForStream: %v", err)
	}
	if track == nil {
		t.Fatal("track is nil")
	}
	if stop == nil {
		t.Fatal("stop is nil")
	}
	waitForCount(t, srv, p, 1) // one subscriber on the shared hub

	stop()
	waitForCount(t, srv, p, 0) // unsubscribed -> pull released
}

func TestTrackForStream_StopIdempotent(t *testing.T) {
	srv, _ := trackTestServer(t, []profile.Profile{passthroughProfile("intercom_web", "cam-1")})
	_, stop, err := srv.TrackForStream("intercom_web")
	if err != nil {
		t.Fatalf("TrackForStream: %v", err)
	}
	stop()
	stop() // must not panic
}

// TestTrackForStream_SharesPull is the key proof: two TrackForStream
// consumers of the same profile share ONE camera pull (Start called
// exactly once), because both go through the shared source registry.
func TestTrackForStream_SharesPull(t *testing.T) {
	p := passthroughProfile("intercom_web", "cam-1")
	srv, starts := trackTestServer(t, []profile.Profile{p})

	_, stop1, err := srv.TrackForStream("intercom_web")
	if err != nil {
		t.Fatalf("first TrackForStream: %v", err)
	}
	_, stop2, err := srv.TrackForStream("intercom_web")
	if err != nil {
		t.Fatalf("second TrackForStream: %v", err)
	}
	waitForCount(t, srv, p, 2) // both attached to the same hub

	if got := starts.Load(); got != 1 {
		t.Errorf("source Start called %d times, want exactly 1 (shared pull)", got)
	}

	stop1()
	stop2()
}
