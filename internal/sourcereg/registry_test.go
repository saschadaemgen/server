package sourcereg

import (
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"carvilon.local/stream/internal/source"
)

// fakeSource is a minimal [source.VideoSource] implementation that
// records its lifecycle so tests can assert "pull only when watched".
type fakeSource struct {
	key       Key
	startCnt  *atomic.Int64
	closeCnt  *atomic.Int64
	framesCh  chan source.AccessUnit
	closeOnce sync.Once
}

func (f *fakeSource) Start(ctx context.Context) error {
	f.startCnt.Add(1)
	return nil
}

func (f *fakeSource) Frames() <-chan source.AccessUnit { return f.framesCh }
func (f *fakeSource) Params() source.H264Params        { return source.H264Params{} }

func (f *fakeSource) Close() error {
	f.closeOnce.Do(func() {
		f.closeCnt.Add(1)
		close(f.framesCh)
	})
	return nil
}

// counts tracks per-key start/close calls across all sources the test
// factory produced. Useful for "0 pull bei 0 viewer"-Asserts.
type counts struct {
	starts atomic.Int64
	closes atomic.Int64
}

// newTestRegistry returns a Registry whose factory produces fakeSources
// that share a per-key counter. The counters map is also returned so the
// test can read it.
func newTestRegistry(t *testing.T) (*Registry, map[Key]*counts) {
	t.Helper()
	var mu sync.Mutex
	counters := make(map[Key]*counts)

	factory := func(k Key) (source.VideoSource, error) {
		mu.Lock()
		c, ok := counters[k]
		if !ok {
			c = &counts{}
			counters[k] = c
		}
		mu.Unlock()
		return &fakeSource{
			key:      k,
			startCnt: &c.starts,
			closeCnt: &c.closes,
			framesCh: make(chan source.AccessUnit, 4),
		}, nil
	}
	return New(factory, log.New(io.Discard, "", 0)), counters
}

func TestRegistry_HubForBuildsLazy(t *testing.T) {
	r, counters := newTestRegistry(t)
	defer r.Close()

	keyA := Key{CameraID: "cam-a", Quality: "high"}

	// Just calling HubFor must not start a source — only Subscribe does.
	_ = r.HubFor(keyA)
	if !r.Has(keyA) {
		t.Error("Has(keyA) should be true after HubFor")
	}
	time.Sleep(20 * time.Millisecond) // let any spurious goroutines run

	if c := counters[keyA]; c != nil && c.starts.Load() != 0 {
		t.Errorf("camera A started %d times before any subscriber — want 0", c.starts.Load())
	}
}

func TestRegistry_NoPullWithoutSubscriber(t *testing.T) {
	r, counters := newTestRegistry(t)
	defer r.Close()

	// Register hubs for three cameras but never subscribe.
	for _, c := range []string{"cam-a", "cam-b", "cam-c"} {
		_ = r.HubFor(Key{CameraID: c, Quality: "high"})
	}
	time.Sleep(50 * time.Millisecond)

	for k, c := range counters {
		if c.starts.Load() != 0 {
			t.Errorf("%s: %d starts without any subscriber — want 0", k, c.starts.Load())
		}
	}
}

func TestRegistry_FirstSubscribeStartsPull(t *testing.T) {
	r, counters := newTestRegistry(t)
	defer r.Close()

	keyA := Key{CameraID: "cam-a", Quality: "high"}
	hubA := r.HubFor(keyA)

	sub, err := hubA.Subscribe()
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	deadline := time.After(time.Second)
	for counters[keyA] == nil || counters[keyA].starts.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("source never started after Subscribe")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestRegistry_LastUnsubscribeStopsPull(t *testing.T) {
	r, counters := newTestRegistry(t)
	defer r.Close()

	keyA := Key{CameraID: "cam-a", Quality: "high"}
	hubA := r.HubFor(keyA)

	sub, _ := hubA.Subscribe()
	// Wait for start.
	deadline := time.After(time.Second)
	for counters[keyA] == nil || counters[keyA].starts.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("source never started")
		case <-time.After(10 * time.Millisecond):
		}
	}

	sub.Close()
	// Wait for close.
	deadline = time.After(time.Second)
	for counters[keyA].closes.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("source never closed after last subscriber left")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestRegistry_TwoCamerasIndependentLifecycle(t *testing.T) {
	r, counters := newTestRegistry(t)
	defer r.Close()

	keyA := Key{CameraID: "cam-a", Quality: "high"}
	keyB := Key{CameraID: "cam-b", Quality: "high"}

	subA, _ := r.HubFor(keyA).Subscribe()
	defer subA.Close()

	// Wait for A to start.
	deadline := time.After(time.Second)
	for counters[keyA] == nil || counters[keyA].starts.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("cam-a never started")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// B must have started ZERO times — A's subscribe should not have
	// triggered any pull on B.
	if c := counters[keyB]; c != nil && c.starts.Load() != 0 {
		t.Errorf("cam-b started %d times after cam-a subscribe — must be 0", c.starts.Load())
	}
}

func TestRegistry_SameKeyReturnsSameHub(t *testing.T) {
	r, _ := newTestRegistry(t)
	defer r.Close()

	keyA := Key{CameraID: "cam-a", Quality: "high"}
	h1 := r.HubFor(keyA)
	h2 := r.HubFor(keyA)
	if h1 != h2 {
		t.Error("HubFor with same key returned different hub instances")
	}
}

func TestRegistry_TwoSubscribersOneHubOnePull(t *testing.T) {
	// Two independent consumers attaching to the SAME camera key should
	// share ONE upstream pull. This mirrors "WebRTC + MJPEG on the same
	// camera reuses the gortsplib client".
	r, counters := newTestRegistry(t)
	defer r.Close()

	keyA := Key{CameraID: "cam-a", Quality: "high"}
	hubA := r.HubFor(keyA)

	subWebRTC, _ := hubA.Subscribe()
	subMJPEG, _ := hubA.Subscribe()
	defer subWebRTC.Close()
	defer subMJPEG.Close()

	time.Sleep(50 * time.Millisecond)

	if got := counters[keyA].starts.Load(); got != 1 {
		t.Errorf("cam-a started %d times — want exactly 1 (shared pull)", got)
	}
}

func TestRegistry_DifferentQualityIsDifferentPull(t *testing.T) {
	// A different quality tier IS a different upstream pull (Protect API
	// returns a different URL). Keys differ.
	r, counters := newTestRegistry(t)
	defer r.Close()

	hi := Key{CameraID: "cam-a", Quality: "high"}
	lo := Key{CameraID: "cam-a", Quality: "low"}

	subHi, _ := r.HubFor(hi).Subscribe()
	subLo, _ := r.HubFor(lo).Subscribe()
	defer subHi.Close()
	defer subLo.Close()

	time.Sleep(50 * time.Millisecond)

	if counters[hi].starts.Load() != 1 {
		t.Errorf("high tier starts = %d, want 1", counters[hi].starts.Load())
	}
	if counters[lo].starts.Load() != 1 {
		t.Errorf("low tier starts = %d, want 1", counters[lo].starts.Load())
	}
}

func TestRegistry_FactoryErrorPropagated(t *testing.T) {
	wantErr := errors.New("factory boom")
	factory := func(Key) (source.VideoSource, error) { return nil, wantErr }
	r := New(factory, log.New(io.Discard, "", 0))
	defer r.Close()

	hubA := r.HubFor(Key{CameraID: "cam-a", Quality: "high"})
	_, err := hubA.Subscribe()
	if !errors.Is(err, wantErr) {
		t.Errorf("Subscribe err = %v, want chain containing %v", err, wantErr)
	}
}

func TestRegistry_FactoryIsNilPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for nil factory")
		}
	}()
	_ = New(nil, nil)
}

func TestRegistry_CloseShutsDownAllHubs(t *testing.T) {
	r, counters := newTestRegistry(t)

	keyA := Key{CameraID: "cam-a", Quality: "high"}
	sub, _ := r.HubFor(keyA).Subscribe()
	// Wait for source to start.
	deadline := time.After(time.Second)
	for counters[keyA] == nil || counters[keyA].starts.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("source never started")
		case <-time.After(10 * time.Millisecond):
		}
	}
	_ = sub // keep alive until Close

	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if counters[keyA].closes.Load() == 0 {
		t.Error("Close did not propagate to active source")
	}
}

func TestKey_String(t *testing.T) {
	// S6-12: Key.String() now includes the Encryption mode so log
	// lines disambiguate same-camera/same-quality pulls that differ
	// only in transport.
	k := Key{CameraID: "abc", Quality: "high", Encryption: "tls"}
	if got := k.String(); got != "abc:high/tls" {
		t.Errorf("String() = %q, want %q", got, "abc:high/tls")
	}
}

// TestKey_DistinctEncryptionGetsDistinctHub is the S6-12 anti-mix-up
// canary. Two profiles on the same camera/quality but with different
// encryption modes MUST land in different map slots in the Registry,
// otherwise the second subscriber would silently inherit the first's
// transport (and either fail to decrypt or fail to authenticate
// every packet).
func TestKey_DistinctEncryptionGetsDistinctHub(t *testing.T) {
	tlsKey := Key{CameraID: "abc", Quality: "high", Encryption: "tls"}
	srtpKey := Key{CameraID: "abc", Quality: "high", Encryption: "srtp"}
	if tlsKey == srtpKey {
		t.Errorf("two Keys with different Encryption compared equal (%+v); they must hash to different map slots", tlsKey)
	}
}
