package hub

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"carvilon.local/stream/internal/source"
)

// --- fakeSource ---------------------------------------------------------------

// fakeSource is a [source.VideoSource] with a hand-driven frames channel.
// It lets tests inject AUs, observe start/close counts, and inject
// upstream-end / start-fail behaviours.
type fakeSource struct {
	frames    chan source.AccessUnit
	startErr  error
	startCnt  atomic.Int64
	closeCnt  atomic.Int64
	closeOnce sync.Once
}

func newFakeSource(bufSize int) *fakeSource {
	return &fakeSource{frames: make(chan source.AccessUnit, bufSize)}
}

func (f *fakeSource) Start(ctx context.Context) error {
	f.startCnt.Add(1)
	return f.startErr
}

func (f *fakeSource) Frames() <-chan source.AccessUnit { return f.frames }
func (f *fakeSource) Params() source.H264Params        { return source.H264Params{} }

func (f *fakeSource) Close() error {
	f.closeCnt.Add(1)
	f.closeOnce.Do(func() { close(f.frames) })
	return nil
}

// endUpstream simulates the source dying upstream (e.g. RTSP connection
// drop). Routed through the same sync.Once as Close so a subsequent
// Close from the hub does not double-close.
func (f *fakeSource) endUpstream() {
	f.closeOnce.Do(func() { close(f.frames) })
}

// --- helpers -----------------------------------------------------------------

func quietLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// expectAU drains one AU off the channel within timeout, failing if it
// never arrives or the channel closes first.
func expectAU(t *testing.T, ch <-chan source.AccessUnit, timeout time.Duration) source.AccessUnit {
	t.Helper()
	select {
	case au, ok := <-ch:
		if !ok {
			t.Fatalf("channel closed unexpectedly")
		}
		return au
	case <-time.After(timeout):
		t.Fatalf("no AU within %s", timeout)
		return source.AccessUnit{}
	}
}

// expectClosed drains anything left in the channel and asserts it ends.
func expectClosed(t *testing.T, ch <-chan source.AccessUnit, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
			// drain and keep waiting for close
		case <-deadline:
			t.Fatalf("channel not closed within %s", timeout)
		}
	}
}

// makeAU constructs an AccessUnit with a single fake NAL.
func makeAU(pts int64, isKey bool) source.AccessUnit {
	return source.AccessUnit{
		NALUs:      [][]byte{{byte(pts)}},
		PTS:        pts,
		IsKeyframe: isKey,
	}
}

// --- tests --------------------------------------------------------------------

func TestHub_SubscribeStartsSourceOnce(t *testing.T) {
	src := newFakeSource(10)
	factory := func() (source.VideoSource, error) { return src, nil }
	h := New(factory, Options{Logger: quietLogger()})
	defer h.Close()

	subA, err := h.Subscribe()
	if err != nil {
		t.Fatalf("Subscribe A: %v", err)
	}
	defer subA.Close()

	subB, err := h.Subscribe()
	if err != nil {
		t.Fatalf("Subscribe B: %v", err)
	}
	defer subB.Close()

	if got := src.startCnt.Load(); got != 1 {
		t.Errorf("source Start called %d times, want exactly 1", got)
	}
}

func TestHub_OneAUReachesAllSubscribers(t *testing.T) {
	src := newFakeSource(10)
	h := New(func() (source.VideoSource, error) { return src, nil }, Options{Logger: quietLogger()})
	defer h.Close()

	subA, _ := h.Subscribe()
	subB, _ := h.Subscribe()
	defer subA.Close()
	defer subB.Close()

	src.frames <- makeAU(1000, false)

	a := expectAU(t, subA.Frames(), time.Second)
	b := expectAU(t, subB.Frames(), time.Second)
	if a.PTS != 1000 || b.PTS != 1000 {
		t.Errorf("got A=%d B=%d, want both 1000", a.PTS, b.PTS)
	}
}

func TestHub_SlowSubscriberDoesNotBlockOthers(t *testing.T) {
	src := newFakeSource(100)
	h := New(func() (source.VideoSource, error) { return src, nil }, Options{
		Logger:           quietLogger(),
		SubscriberBuffer: 2, // tiny on purpose for the slow side
	})
	defer h.Close()

	slow, _ := h.Subscribe() // intentionally never reads
	fast, _ := h.Subscribe()
	defer slow.Close()
	defer fast.Close()

	// Producer: paced to mimic ~30 fps so the fast subscriber can keep up
	// without overflowing its own (also-small) channel. The slow one
	// never reads, so it must drop after its buffer fills.
	const totalProduced = 60
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for i := 0; i < totalProduced; i++ {
			select {
			case src.frames <- makeAU(int64(i+1), false):
			case <-stop:
				return
			}
			select {
			case <-time.After(2 * time.Millisecond):
			case <-stop:
				return
			}
		}
	}()

	// The property to prove: the fast subscriber keeps making forward
	// progress despite slow being completely stuck. With paced
	// production, fast should see essentially all frames.
	const wantAtLeast = 50
	received := 0
	deadline := time.After(3 * time.Second)
	for received < wantAtLeast {
		select {
		case _, ok := <-fast.Frames():
			if !ok {
				t.Fatal("fast subscriber channel closed prematurely")
			}
			received++
		case <-deadline:
			t.Fatalf("fast subscriber got only %d frames in 3 s — slow blocked the bus", received)
		}
	}

	// Sanity: slow's channel never grew past its buffer.
	if got := len(slow.frames); got > cap(slow.frames) {
		t.Errorf("slow buffer overflowed: len=%d cap=%d", got, cap(slow.frames))
	}
}

func TestHub_LastUnsubscribeStopsSource(t *testing.T) {
	src := newFakeSource(10)
	h := New(func() (source.VideoSource, error) { return src, nil }, Options{Logger: quietLogger()})
	defer h.Close()

	sub, _ := h.Subscribe()
	if src.closeCnt.Load() != 0 {
		t.Errorf("source closed before any leave")
	}

	sub.Close()

	// Wait briefly for the unsub message to be processed.
	deadline := time.After(time.Second)
	for src.closeCnt.Load() == 0 {
		select {
		case <-deadline:
			t.Fatalf("source not closed after last subscriber left")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestHub_ResubscribeBuildsFreshSource(t *testing.T) {
	var builds atomic.Int64
	factory := func() (source.VideoSource, error) {
		builds.Add(1)
		return newFakeSource(10), nil
	}

	h := New(factory, Options{Logger: quietLogger()})
	defer h.Close()

	sub1, err := h.Subscribe()
	if err != nil {
		t.Fatalf("Subscribe 1: %v", err)
	}
	sub1.Close()

	// give the hub time to process the unsub + close cycle
	time.Sleep(50 * time.Millisecond)

	sub2, err := h.Subscribe()
	if err != nil {
		t.Fatalf("Subscribe 2: %v", err)
	}
	defer sub2.Close()

	if got := builds.Load(); got != 2 {
		t.Errorf("factory invoked %d times, want exactly 2 (one per source lifetime)", got)
	}
}

func TestHub_NewSubscriberGetsCachedIDR(t *testing.T) {
	src := newFakeSource(10)
	h := New(func() (source.VideoSource, error) { return src, nil }, Options{Logger: quietLogger()})
	defer h.Close()

	sub1, _ := h.Subscribe()
	defer sub1.Close()

	// Push an IDR, then a P-frame.
	src.frames <- makeAU(1000, true)
	src.frames <- makeAU(1100, false)

	// Drain sub1 to confirm normal flow.
	_ = expectAU(t, sub1.Frames(), time.Second)
	_ = expectAU(t, sub1.Frames(), time.Second)

	// Now a late joiner. Should immediately get the cached IDR.
	sub2, _ := h.Subscribe()
	defer sub2.Close()

	first := expectAU(t, sub2.Frames(), time.Second)
	if first.PTS != 1000 || !first.IsKeyframe {
		t.Errorf("late subscriber's first AU: pts=%d isKey=%v, want pts=1000 isKey=true",
			first.PTS, first.IsKeyframe)
	}
}

func TestHub_SourceStartErrorPropagated(t *testing.T) {
	wantErr := errors.New("synthetic start failure")
	factory := func() (source.VideoSource, error) {
		s := newFakeSource(10)
		s.startErr = wantErr
		return s, nil
	}

	h := New(factory, Options{Logger: quietLogger()})
	defer h.Close()

	_, err := h.Subscribe()
	if !errors.Is(err, wantErr) {
		t.Errorf("Subscribe err = %v, want chain containing %v", err, wantErr)
	}
}

func TestHub_FactoryErrorPropagated(t *testing.T) {
	wantErr := errors.New("synthetic factory failure")
	factory := func() (source.VideoSource, error) {
		return nil, wantErr
	}

	h := New(factory, Options{Logger: quietLogger()})
	defer h.Close()

	_, err := h.Subscribe()
	if !errors.Is(err, wantErr) {
		t.Errorf("Subscribe err = %v, want chain containing %v", err, wantErr)
	}
}

func TestHub_UpstreamEndClosesAllSubscriberChannels(t *testing.T) {
	src := newFakeSource(10)
	h := New(func() (source.VideoSource, error) { return src, nil }, Options{Logger: quietLogger()})
	defer h.Close()

	subA, _ := h.Subscribe()
	subB, _ := h.Subscribe()

	// Simulate upstream end via the closeOnce-protected helper.
	src.endUpstream()

	expectClosed(t, subA.Frames(), time.Second)
	expectClosed(t, subB.Frames(), time.Second)
}

func TestHub_CloseShutsDownEverything(t *testing.T) {
	src := newFakeSource(10)
	h := New(func() (source.VideoSource, error) { return src, nil }, Options{Logger: quietLogger()})

	sub, _ := h.Subscribe()

	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	expectClosed(t, sub.Frames(), time.Second)
	if src.closeCnt.Load() == 0 {
		t.Errorf("source was not closed on hub shutdown")
	}
}

func TestHub_CloseAfterClose(t *testing.T) {
	h := New(func() (source.VideoSource, error) { return newFakeSource(10), nil }, Options{Logger: quietLogger()})
	if err := h.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestHub_SubscribeAfterCloseReturnsError(t *testing.T) {
	h := New(func() (source.VideoSource, error) { return newFakeSource(10), nil }, Options{Logger: quietLogger()})
	_ = h.Close()

	_, err := h.Subscribe()
	if err == nil {
		t.Error("Subscribe after Close should return an error")
	}
}

// TestHub_ConcurrencyStressIsRaceClean exercises Subscribe / Unsubscribe /
// frame distribution / Close from many goroutines at once. Run with
// `go test -race ./internal/hub/...` to catch data races on the
// subscriber map and the source pointer.
//
// An "anchor" subscriber stays alive the whole test so the source is
// never torn down and rebuilt — we test concurrency around the bus,
// not around the lifecycle (lifecycle is covered by other tests).
func TestHub_ConcurrencyStressIsRaceClean(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in -short mode")
	}

	src := newFakeSource(1000)
	h := New(func() (source.VideoSource, error) { return src, nil }, Options{
		Logger:           quietLogger(),
		SubscriberBuffer: 4,
	})
	defer h.Close()

	// Anchor subscriber: keeps the source alive throughout.
	anchor, err := h.Subscribe()
	if err != nil {
		t.Fatalf("anchor Subscribe: %v", err)
	}
	defer anchor.Close()
	anchorDone := make(chan struct{})
	go func() {
		defer close(anchorDone)
		for range anchor.Frames() {
		}
	}()

	// Frame producer — paced lightly so we don't burn CPU.
	stopProducer := make(chan struct{})
	prodDone := make(chan struct{})
	go func() {
		defer close(prodDone)
		var pts int64
		for {
			select {
			case <-stopProducer:
				return
			case src.frames <- makeAU(pts, pts%30 == 0):
				pts++
			}
		}
	}()

	// Subscriber churn.
	const churnGoroutines = 8
	const churnCycles = 50
	var wg sync.WaitGroup
	for i := 0; i < churnGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < churnCycles; j++ {
				sub, err := h.Subscribe()
				if err != nil {
					return
				}
				for k := 0; k < 5; k++ {
					select {
					case _, ok := <-sub.Frames():
						if !ok {
							sub.Close()
							goto nextCycle
						}
					case <-time.After(100 * time.Millisecond):
					}
				}
				sub.Close()
			nextCycle:
			}
		}()
	}

	wg.Wait()
	close(stopProducer)
	<-prodDone
}

// TestHub_ResubscribeAfterUpstreamEnd verifies that after the source
// disappears (upstream end), a new Subscribe correctly builds a fresh
// source via the factory rather than wedging.
func TestHub_ResubscribeAfterUpstreamEnd(t *testing.T) {
	var (
		mu       sync.Mutex
		current  *fakeSource
		builds   atomic.Int64
		bufBytes bytes.Buffer
	)
	logger := log.New(&bufBytes, "", 0)

	factory := func() (source.VideoSource, error) {
		builds.Add(1)
		mu.Lock()
		current = newFakeSource(10)
		mu.Unlock()
		return current, nil
	}

	h := New(factory, Options{Logger: logger})
	defer h.Close()

	subA, _ := h.Subscribe()
	mu.Lock()
	srcA := current
	mu.Unlock()

	// Source dies upstream.
	srcA.endUpstream()
	expectClosed(t, subA.Frames(), time.Second)

	// Give the hub a moment to settle (it logs "upstream end" and
	// drops the source pointer).
	time.Sleep(50 * time.Millisecond)

	// A new Subscribe should rebuild the source.
	subB, err := h.Subscribe()
	if err != nil {
		t.Fatalf("Subscribe after upstream end: %v", err)
	}
	defer subB.Close()

	if got := builds.Load(); got != 2 {
		t.Errorf("factory invoked %d times after upstream end, want 2", got)
	}

	// Sanity: pump one frame through the new source.
	mu.Lock()
	srcB := current
	mu.Unlock()
	srcB.frames <- makeAU(42, true)
	got := expectAU(t, subB.Frames(), time.Second)
	if got.PTS != 42 {
		t.Errorf("first AU after rebuild pts=%d, want 42", got.PTS)
	}

	// Sentinel log line should be present.
	if !bytes.Contains(bufBytes.Bytes(), []byte("upstream end")) {
		t.Errorf("expected 'upstream end' note in log; got:\n%s", bufBytes.String())
	}
}

func TestSubscriber_CloseIsIdempotent(t *testing.T) {
	src := newFakeSource(10)
	h := New(func() (source.VideoSource, error) { return src, nil }, Options{Logger: quietLogger()})
	defer h.Close()

	sub, _ := h.Subscribe()
	sub.Close()
	sub.Close() // must not panic
	sub.Close() // must not panic
}

func TestHub_FactoryIsNilPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for nil factory")
		}
	}()
	_ = New(nil, Options{})
}

// quick sanity that Subscribe order maps 1:1 to subscriber IDs.
func TestHub_SubscriberIDsAreUnique(t *testing.T) {
	src := newFakeSource(10)
	h := New(func() (source.VideoSource, error) { return src, nil }, Options{Logger: quietLogger()})
	defer h.Close()

	const n = 16
	subs := make([]*Subscriber, n)
	for i := 0; i < n; i++ {
		s, err := h.Subscribe()
		if err != nil {
			t.Fatalf("Subscribe %d: %v", i, err)
		}
		subs[i] = s
	}
	defer func() {
		for _, s := range subs {
			s.Close()
		}
	}()

	seen := map[uint64]bool{}
	for _, s := range subs {
		if seen[s.ID()] {
			t.Errorf("duplicate id %d", s.ID())
		}
		seen[s.ID()] = true
	}
	if len(seen) != n {
		t.Errorf("got %d unique IDs, want %d", len(seen), n)
	}
}

// Ensure the fakeSource and fmt import are exercised (avoid linter noise
// if a test is later commented out).
var _ = fmt.Sprintf
