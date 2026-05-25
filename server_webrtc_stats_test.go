package stream

import (
	"context"
	"io"
	"log"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"carvilon.local/stream/internal/droplog"
	"carvilon.local/stream/internal/hub"
	"carvilon.local/stream/internal/source"
	"carvilon.local/stream/internal/stats"
)

// --- test plumbing ----------------------------------------------------------

// statsFakeSource is a minimal [source.VideoSource] for the feedTrack
// tests: a hand-driven AU channel, closeOnce-protected end. Mirrors
// internal/hub/hub_test.go's fakeSource but kept local so this test
// stays self-contained.
type statsFakeSource struct {
	frames    chan source.AccessUnit
	startCnt  atomic.Int64
	closeCnt  atomic.Int64
	closeDone atomic.Bool
}

func newStatsFakeSource(buf int) *statsFakeSource {
	return &statsFakeSource{frames: make(chan source.AccessUnit, buf)}
}

func (f *statsFakeSource) Start(ctx context.Context) error { f.startCnt.Add(1); return nil }
func (f *statsFakeSource) Frames() <-chan source.AccessUnit { return f.frames }
func (f *statsFakeSource) Params() source.H264Params        { return source.H264Params{} }
func (f *statsFakeSource) Close() error {
	f.closeCnt.Add(1)
	if f.closeDone.CompareAndSwap(false, true) {
		close(f.frames)
	}
	return nil
}

// newWebRTCFeedTestRig spins up: one hub.Hub fed by a fake source, one
// real *stats.Client registered into a fresh stats.Registry, one
// unbound webrtc.TrackLocalStaticSample (WriteSample is a no-op
// without a peer — pion returns nil early when packetizer is nil),
// and a droplog.Counter pointed at io.Discard. The returned cleanup
// closes the hub. Used by every feedTrack test below.
type webrtcFeedRig struct {
	src      *statsFakeSource
	hub      *hub.Hub
	sub      *hub.Subscriber
	reg      *stats.Registry
	client   *stats.Client
	track    *webrtc.TrackLocalStaticSample
	drops    *droplog.Counter
	cleanup  func()
}

func newWebRTCFeedRig(t *testing.T) *webrtcFeedRig {
	t.Helper()
	src := newStatsFakeSource(4)
	quiet := log.New(io.Discard, "", 0)
	h := hub.New(func() (source.VideoSource, error) { return src, nil }, hub.Options{
		Logger:           quiet,
		SubscriberBuffer: 4,
	})
	sub, err := h.Subscribe()
	if err != nil {
		t.Fatalf("hub.Subscribe: %v", err)
	}
	reg := stats.New()
	sc := reg.Register("intercom_web", "h264_passthrough", "10.0.0.7:54321")
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000},
		"video-test",
		"stream-test",
	)
	if err != nil {
		t.Fatalf("NewTrackLocalStaticSample: %v", err)
	}
	drops := &droplog.Counter{Logger: quiet, Label: "test"}
	return &webrtcFeedRig{
		src:    src,
		hub:    h,
		sub:    sub,
		reg:    reg,
		client: sc,
		track:  track,
		drops:  drops,
		cleanup: func() {
			sub.Close()
			_ = h.Close()
		},
	}
}

// runFeedTrack starts feedTrack in a goroutine and returns a channel
// that closes when feedTrack returns. The caller drives the test by
// pushing AUs into rig.src.frames and / or closing the subscriber.
func runFeedTrack(t *testing.T, rig *webrtcFeedRig) <-chan struct{} {
	t.Helper()
	srv := freshServer(t, ServerOptions{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.feedTrack(rig.sub, rig.track, rig.drops, rig.client)
	}()
	return done
}

// pushAU sends one AU through the fake source into the hub.
func pushAU(t *testing.T, rig *webrtcFeedRig, payload []byte, pts int64) {
	t.Helper()
	select {
	case rig.src.frames <- source.AccessUnit{NALUs: [][]byte{payload}, PTS: pts}:
	case <-time.After(time.Second):
		t.Fatalf("fake source channel full")
	}
}

// waitDone waits for feedTrack to exit, failing on timeout.
func waitDone(t *testing.T, done <-chan struct{}, timeout time.Duration, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("feedTrack did not exit within %s (%s)", timeout, what)
	}
}

// --- tests ------------------------------------------------------------------

// TestFeedTrack_RecordsFramesIntoStatsClient asserts the S6-15
// instrumentation: each AU successfully written to the WebRTC track
// produces one RecordFrame call on the per-viewer stats client, with
// byte count equal to the Annex-B-marshalled payload length. The
// /stream/stats snapshot is checked end-to-end.
func TestFeedTrack_RecordsFramesIntoStatsClient(t *testing.T) {
	rig := newWebRTCFeedRig(t)
	defer rig.cleanup()
	done := runFeedTrack(t, rig)

	// Push three AUs with distinct payloads.
	pushAU(t, rig, []byte{0x65, 0x88, 0x99}, 0)     // IDR-ish first NAL byte
	pushAU(t, rig, []byte{0x41, 0xaa, 0xbb}, 3000)  // P-frame-ish
	pushAU(t, rig, []byte{0x41, 0xcc, 0xdd}, 6000)
	// Each AU is one NAL, marshalled as 4-byte start code + NAL bytes
	// = 4 + 3 = 7 bytes per frame.
	const wantBytesPerFrame = 7

	// Give the hub run-loop and feedTrack a moment to drain.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap := rig.reg.Snapshot()
		if len(snap.Clients) == 1 && snap.Clients[0].FramesSent >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	snap := rig.reg.Snapshot()
	if snap.Global.Clients != 1 {
		t.Fatalf("Global.Clients = %d, want 1", snap.Global.Clients)
	}
	cs := snap.Clients[0]
	if cs.Profile != "intercom_web" || cs.Codec != "h264_passthrough" {
		t.Errorf("client identification wrong: %+v", cs)
	}
	if cs.FramesSent != 3 {
		t.Errorf("FramesSent = %d, want 3", cs.FramesSent)
	}
	if cs.BytesSent != int64(wantBytesPerFrame*3) {
		t.Errorf("BytesSent = %d, want %d", cs.BytesSent, wantBytesPerFrame*3)
	}

	// Profile-block aggregation must agree.
	ps, ok := snap.Profiles["intercom_web"]
	if !ok {
		t.Fatalf("Profiles missing intercom_web: %+v", snap.Profiles)
	}
	if ps.Clients != 1 || ps.Codec != "h264_passthrough" {
		t.Errorf("ProfileSnapshot wrong: %+v", ps)
	}

	rig.sub.Close()
	waitDone(t, done, time.Second, "after sub.Close")
}

// TestFeedTrack_ReturnsOnSubscriberClose asserts the "primary
// teardown path" — when the hub closes the subscriber's frames
// channel (the path taken by OnConnectionStateChange in handleOffer),
// feedTrack exits promptly. The caller's deferred Unregister then
// removes the client from /stream/stats.
func TestFeedTrack_ReturnsOnSubscriberClose(t *testing.T) {
	rig := newWebRTCFeedRig(t)
	defer rig.cleanup()
	done := runFeedTrack(t, rig)

	// No frames pushed; close immediately.
	rig.sub.Close()
	waitDone(t, done, time.Second, "after sub.Close")
}

// TestFeedTrack_ReturnsOnIdleTimeout asserts the defensive watchdog:
// if no AU arrives for webrtcIdleTimeout, feedTrack exits even though
// the subscriber's channel is still open. Protects /stream/stats from
// ghost entries when pion's OnConnectionStateChange callback fails to
// fire on disconnect.
func TestFeedTrack_ReturnsOnIdleTimeout(t *testing.T) {
	// Patch the timeout to something a unit test can wait on.
	old := webrtcIdleTimeout
	webrtcIdleTimeout = 50 * time.Millisecond
	t.Cleanup(func() { webrtcIdleTimeout = old })

	rig := newWebRTCFeedRig(t)
	defer rig.cleanup()
	done := runFeedTrack(t, rig)

	// Push nothing — let the watchdog fire.
	waitDone(t, done, 2*time.Second, "after idle timeout")
}

// TestFeedTrack_IdleTimerResetsOnFrames asserts the watchdog timer is
// reset on every successful write, so a healthy stream never trips it
// even if individual frames are slower than the static webrtcIdleTimeout
// would suggest at first glance.
func TestFeedTrack_IdleTimerResetsOnFrames(t *testing.T) {
	old := webrtcIdleTimeout
	webrtcIdleTimeout = 80 * time.Millisecond
	t.Cleanup(func() { webrtcIdleTimeout = old })

	rig := newWebRTCFeedRig(t)
	defer rig.cleanup()
	done := runFeedTrack(t, rig)

	// Push a frame every 30 ms for 200 ms — never long enough to trip
	// the (80 ms) idle window if the reset logic works.
	stopFeeding := time.After(200 * time.Millisecond)
	ticker := time.NewTicker(30 * time.Millisecond)
	defer ticker.Stop()
	var pushed int
feed:
	for {
		select {
		case <-stopFeeding:
			break feed
		case <-ticker.C:
			pushed++
			pushAU(t, rig, []byte{0x41, byte(pushed)}, int64(pushed)*3000)
		case <-done:
			t.Fatalf("feedTrack returned during healthy streaming (after %d frames)", pushed)
		}
	}

	// Stop feeding; the watchdog should now fire ~80 ms later.
	waitDone(t, done, 500*time.Millisecond, "after stopping the feed")

	snap := rig.reg.Snapshot()
	if len(snap.Clients) != 1 || snap.Clients[0].FramesSent < int64(pushed) {
		t.Errorf("FramesSent did not reach %d: snapshot=%+v", pushed, snap.Clients)
	}
}

// TestFeedTrack_NilStatsClientIsSafe asserts the nil-stats path:
// passing a nil *stats.Client (the production path when the server is
// built without a stats registry) must not crash. The track is still
// fed normally; only the counters are skipped.
func TestFeedTrack_NilStatsClientIsSafe(t *testing.T) {
	rig := newWebRTCFeedRig(t)
	defer rig.cleanup()

	srv := freshServer(t, ServerOptions{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.feedTrack(rig.sub, rig.track, rig.drops, nil) // nil stats client
	}()

	pushAU(t, rig, []byte{0x65, 0x01, 0x02}, 0)
	// Let the AU drain.
	time.Sleep(50 * time.Millisecond)

	rig.sub.Close()
	waitDone(t, done, time.Second, "after sub.Close")
}
