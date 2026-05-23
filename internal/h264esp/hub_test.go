package h264esp

import (
	"errors"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"carvilon.local/stream/internal/source"
)

// --- fakes ------------------------------------------------------------------

type fakeSource struct {
	mu     sync.Mutex
	subs   []*fakeSrcSub
	closed bool
}

func newFakeSource() *fakeSource { return &fakeSource{} }

func (f *fakeSource) Subscribe() (SourceSubscriber, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, errors.New("fake source closed")
	}
	s := &fakeSrcSub{frames: make(chan source.AccessUnit, 10), parent: f}
	f.subs = append(f.subs, s)
	return s, nil
}

func (f *fakeSource) subscriberCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs)
}

type fakeSrcSub struct {
	frames chan source.AccessUnit
	parent *fakeSource
	once   sync.Once
}

func (s *fakeSrcSub) Frames() <-chan source.AccessUnit { return s.frames }

func (s *fakeSrcSub) Close() {
	s.once.Do(func() {
		s.parent.mu.Lock()
		for i, x := range s.parent.subs {
			if x == s {
				s.parent.subs = append(s.parent.subs[:i], s.parent.subs[i+1:]...)
				break
			}
		}
		s.parent.mu.Unlock()
		close(s.frames)
	})
}

// fakeEncoder lets us drive the hub without spawning ffmpeg. emit
// pushes a pre-formed Annex-B AU onto the output as if the splitter
// had assembled one.
type fakeEncoder struct {
	label   string
	spec    EncodeSpec
	in      chan source.AccessUnit
	out     chan []byte
	started atomic.Bool
	closed  atomic.Bool
}

func newFakeEncoder(label string, spec EncodeSpec) *fakeEncoder {
	return &fakeEncoder{
		label: label,
		spec:  spec,
		in:    make(chan source.AccessUnit, 8),
		out:   make(chan []byte, 4),
	}
}

func (e *fakeEncoder) Start() error                     { e.started.Store(true); return nil }
func (e *fakeEncoder) Input() chan<- source.AccessUnit  { return e.in }
func (e *fakeEncoder) AUs() <-chan []byte               { return e.out }
func (e *fakeEncoder) Close() error {
	if e.closed.CompareAndSwap(false, true) {
		close(e.out)
	}
	return nil
}
func (e *fakeEncoder) emit(au []byte) { e.out <- au }

// inputForTest exposes the unwrapped input channel so test goroutines
// can verify what the forwarder pumped into the encoder. Not part of
// the encoderIface contract.
func (e *fakeEncoder) inputForTest() <-chan source.AccessUnit { return e.in }

func quietHubLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// testHub wires up entry-resolution for one h264_cbp profile backed
// by the given source. Returned encs is keyed by profile name.
func testHub(t *testing.T, src *fakeSource, encs map[string]*fakeEncoder) *Hub {
	t.Helper()
	spec := EncodeSpec{Width: 800, Height: 1280, FPS: 15, Quality: 26}
	resolver := func(name string) (Entry, error) {
		if name != "h264_cbp" {
			return Entry{}, errors.New("unknown profile")
		}
		return Entry{Spec: spec, Source: src}, nil
	}
	factory := func(label string, sp EncodeSpec) (encoderIface, error) {
		e := newFakeEncoder(label, sp)
		encs[label] = e
		return e, nil
	}
	h, err := NewHub(HubOptions{
		EntryFor:         resolver,
		Logger:           quietHubLogger(),
		SubscriberBuffer: 8,
		EncoderFactory:   factory,
	})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// --- happy path -------------------------------------------------------------

func TestHub_SubscribeStartsEncoderAndUpstreamPull(t *testing.T) {
	src := newFakeSource()
	encs := map[string]*fakeEncoder{}
	h := testHub(t, src, encs)

	if src.subscriberCount() != 0 {
		t.Fatal("source has subscribers before any Subscribe")
	}
	sub, err := h.Subscribe("h264_cbp")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	if src.subscriberCount() != 1 {
		t.Errorf("upstream subscriberCount = %d, want 1", src.subscriberCount())
	}
	if e := encs["h264_cbp"]; e == nil || !e.started.Load() {
		t.Errorf("encoder for h264_cbp not started: %v", e)
	}
}

func TestHub_OneEncoderForManySubscribers(t *testing.T) {
	// THE briefing requirement: fan-out = ONE transcode, N clients.
	src := newFakeSource()
	encs := map[string]*fakeEncoder{}
	h := testHub(t, src, encs)

	subs := []*Subscriber{}
	for i := 0; i < 3; i++ {
		s, err := h.Subscribe("h264_cbp")
		if err != nil {
			t.Fatalf("Subscribe %d: %v", i, err)
		}
		subs = append(subs, s)
	}
	defer func() {
		for _, s := range subs {
			s.Close()
		}
	}()

	// Still just one encoder, one upstream pull.
	if got := src.subscriberCount(); got != 1 {
		t.Errorf("upstream subscriberCount = %d, want 1 (fan-out broken)", got)
	}
	if got := len(encs); got != 1 {
		t.Errorf("encoder count = %d, want 1", got)
	}

	// Emit one AU; all three viewers see it.
	encs["h264_cbp"].emit([]byte{0x00, 0x00, 0x00, 0x01, 0x65}) // fake IDR

	for i, s := range subs {
		select {
		case got := <-s.Frames():
			if len(got) == 0 {
				t.Errorf("sub %d got empty AU", i)
			}
		case <-time.After(time.Second):
			t.Errorf("sub %d did not receive AU within 1s", i)
		}
	}
}

func TestHub_LastSubscriberCleanup(t *testing.T) {
	// Bedarfsgesteuert (briefing): last viewer leaves -> encoder + upstream gone.
	src := newFakeSource()
	encs := map[string]*fakeEncoder{}
	h := testHub(t, src, encs)

	sub, err := h.Subscribe("h264_cbp")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if src.subscriberCount() != 1 {
		t.Fatalf("setup: src subs = %d", src.subscriberCount())
	}

	sub.Close()

	// Hub takes a moment to remove the session.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if src.subscriberCount() == 0 && encs["h264_cbp"].closed.Load() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("teardown didn't complete: src subs=%d encoder closed=%v",
		src.subscriberCount(), encs["h264_cbp"].closed.Load())
}

func TestHub_SubscribeUnknownProfileErrors(t *testing.T) {
	src := newFakeSource()
	h := testHub(t, src, map[string]*fakeEncoder{})
	if _, err := h.Subscribe("nonexistent"); err == nil {
		t.Error("expected error for unknown profile")
	}
}

func TestHub_AUFanOut_DropSlowViewer(t *testing.T) {
	// Drop-statt-buffer per client (briefing). A wedged viewer must
	// NOT block the encoder thread or other viewers.
	src := newFakeSource()
	encs := map[string]*fakeEncoder{}
	h := testHub(t, src, encs)

	slow, _ := h.Subscribe("h264_cbp")
	fast, _ := h.Subscribe("h264_cbp")
	defer slow.Close()
	defer fast.Close()

	// Don't read from `slow`. Push enough AUs to overflow its buffer
	// (subBufSize=8 in the test hub).
	enc := encs["h264_cbp"]
	for i := 0; i < 30; i++ {
		enc.emit([]byte{0x00, 0x00, 0x00, 0x01, byte(0x40 + i)})
	}

	// `fast` must receive frames despite `slow` being wedged.
	deadline := time.Now().Add(2 * time.Second)
	received := 0
	for time.Now().Before(deadline) && received < 5 {
		select {
		case <-fast.Frames():
			received++
		case <-time.After(100 * time.Millisecond):
		}
	}
	if received < 5 {
		t.Errorf("fast viewer only got %d frames; want >=5 (slow viewer should not block)", received)
	}
}

func TestHub_ForwarderPumpsAUsToEncoder(t *testing.T) {
	// AUs that arrive on the upstream source land on encoder.Input.
	src := newFakeSource()
	encs := map[string]*fakeEncoder{}
	h := testHub(t, src, encs)

	sub, _ := h.Subscribe("h264_cbp")
	defer sub.Close()

	srcSub := src.subs[0]
	srcSub.frames <- source.AccessUnit{
		NALUs:      [][]byte{{0x65, 0x88, 0x80, 0x10}},
		PTS:        1000,
		IsKeyframe: true,
	}

	enc := encs["h264_cbp"]
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-enc.inputForTest():
			return // got it
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Error("encoder.Input never received an AU from the forwarder")
}

func TestHub_EncoderEndClosesSubscribers(t *testing.T) {
	src := newFakeSource()
	encs := map[string]*fakeEncoder{}
	h := testHub(t, src, encs)

	sub, _ := h.Subscribe("h264_cbp")
	defer sub.Close()

	// Simulate ffmpeg dying: close the encoder output channel.
	close(encs["h264_cbp"].out)
	encs["h264_cbp"].closed.Store(true)

	select {
	case _, ok := <-sub.Frames():
		if ok {
			t.Error("sub received a value after encoder end; want channel closed")
		}
	case <-time.After(2 * time.Second):
		t.Error("sub channel did not close within 2s after encoder ended")
	}
}

func TestHub_CloseTearsDownEverything(t *testing.T) {
	src := newFakeSource()
	encs := map[string]*fakeEncoder{}
	h := testHub(t, src, encs)

	for i := 0; i < 3; i++ {
		s, err := h.Subscribe("h264_cbp")
		if err != nil {
			t.Fatalf("Subscribe %d: %v", i, err)
		}
		_ = s
	}

	if err := h.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if src.subscriberCount() != 0 {
		t.Errorf("source still has %d subscribers after Close", src.subscriberCount())
	}
	if !encs["h264_cbp"].closed.Load() {
		t.Error("encoder still open after Close")
	}
}

// TestHub_SpecChangeRetiresOldSession — S6-10 counterpart to the mjpeg
// test of the same name. PUT /api/profiles/h264_cbp changes
// fps / CRF / size while an existing client is still connected; the
// next Subscribe must spawn a fresh encoder with the new spec, not
// join the long-lived old one.
func TestHub_SpecChangeRetiresOldSession(t *testing.T) {
	src := newFakeSource()

	var (
		specMu      sync.Mutex
		currentSpec = EncodeSpec{Width: 800, Height: 1280, FPS: 15, Quality: 26}
	)
	getSpec := func() EncodeSpec {
		specMu.Lock()
		defer specMu.Unlock()
		return currentSpec
	}
	setSpec := func(s EncodeSpec) {
		specMu.Lock()
		defer specMu.Unlock()
		currentSpec = s
	}

	var encoders []*fakeEncoder
	factory := func(label string, sp EncodeSpec) (encoderIface, error) {
		fe := newFakeEncoder(label, sp)
		encoders = append(encoders, fe)
		return fe, nil
	}

	h, err := NewHub(HubOptions{
		EntryFor: func(name string) (Entry, error) {
			if name != "h264_cbp" {
				return Entry{}, errors.New("unknown profile")
			}
			return Entry{Spec: getSpec(), Source: src}, nil
		},
		EncoderFactory:   factory,
		Logger:           quietHubLogger(),
		SubscriberBuffer: 4,
	})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	defer h.Close()

	sub1, err := h.Subscribe("h264_cbp")
	if err != nil {
		t.Fatalf("Subscribe #1: %v", err)
	}
	if len(encoders) != 1 {
		t.Fatalf("after first Subscribe: encoder count = %d, want 1", len(encoders))
	}
	if got := encoders[0].spec.FPS; got != 15 {
		t.Errorf("encoder #1 FPS = %d, want 15", got)
	}

	// Simulate PUT /api/profiles/h264_cbp with new fps + CRF.
	setSpec(EncodeSpec{Width: 800, Height: 1280, FPS: 30, Quality: 22})

	sub2, err := h.Subscribe("h264_cbp")
	if err != nil {
		t.Fatalf("Subscribe #2: %v", err)
	}
	if len(encoders) != 2 {
		t.Fatalf("after spec-change Subscribe: encoder count = %d, want 2 (fresh encoder)", len(encoders))
	}
	if got := encoders[1].spec.FPS; got != 30 {
		t.Errorf("encoder #2 FPS = %d, want 30 (new spec)", got)
	}
	if got := encoders[1].spec.Quality; got != 22 {
		t.Errorf("encoder #2 Quality = %d, want 22", got)
	}

	// Old subscriber still receives AUs from encoder #1; new from #2.
	encoders[0].emit([]byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x01})
	select {
	case <-sub1.Frames():
	case <-time.After(time.Second):
		t.Error("sub1 did not receive from OLD encoder")
	}
	encoders[1].emit([]byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x02})
	select {
	case <-sub2.Frames():
	case <-time.After(time.Second):
		t.Error("sub2 did not receive from NEW encoder")
	}

	sub1.Close()
	sub2.Close()
}

// TestHub_SpecUnchangedJoinsExistingSession — inverse: same spec
// means the fan-out invariant holds.
func TestHub_SpecUnchangedJoinsExistingSession(t *testing.T) {
	src := newFakeSource()
	encs := make(map[string]*fakeEncoder)
	h := testHub(t, src, encs)
	defer h.Close()

	var subs []*Subscriber
	for i := 0; i < 4; i++ {
		s, err := h.Subscribe("h264_cbp")
		if err != nil {
			t.Fatalf("Subscribe #%d: %v", i, err)
		}
		subs = append(subs, s)
	}
	defer func() {
		for _, s := range subs {
			s.Close()
		}
	}()

	if len(encs) != 1 {
		t.Errorf("encoder count = %d, want 1 (same-spec subscribes must share)", len(encs))
	}
}
