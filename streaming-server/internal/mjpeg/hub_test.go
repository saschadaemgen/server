package mjpeg

import (
	"errors"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"carvilon.local/stream/internal/profile"
	"carvilon.local/stream/internal/source"
)

// --- fake upstream source -----------------------------------------------------

type fakeSource struct {
	mu       sync.Mutex
	subs     []*fakeSrcSub
	closed   bool
	subCount atomic.Int64
}

func newFakeSource() *fakeSource { return &fakeSource{} }

func (f *fakeSource) Subscribe() (SourceSubscriber, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, errors.New("fake source closed")
	}
	f.subCount.Add(1)
	s := &fakeSrcSub{frames: make(chan source.AccessUnit, 10), parent: f}
	f.subs = append(f.subs, s)
	return s, nil
}

func (f *fakeSource) broadcast(au source.AccessUnit) {
	f.mu.Lock()
	subs := make([]*fakeSrcSub, len(f.subs))
	copy(subs, f.subs)
	f.mu.Unlock()
	for _, s := range subs {
		select {
		case s.frames <- au:
		default:
		}
	}
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
		// Remove from parent's list (best effort).
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

// --- fake encoder -------------------------------------------------------------

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

func (e *fakeEncoder) Start() error {
	e.started.Store(true)
	return nil
}
func (e *fakeEncoder) Input() chan<- source.AccessUnit { return e.in }
func (e *fakeEncoder) JPEGs() <-chan []byte             { return e.out }
func (e *fakeEncoder) Close() error {
	if e.closed.CompareAndSwap(false, true) {
		close(e.out)
	}
	return nil
}

// emit pushes a JPEG frame onto the encoder output (as if ffmpeg
// produced one).
func (e *fakeEncoder) emit(frame []byte) {
	e.out <- frame
}

// --- helpers ------------------------------------------------------------------

func quietHubLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// testProfiles wires up entry-resolution for the standard pair of
// browser/esp profiles, all backed by the given source. Encoders are
// stored in `encs` keyed by profile name for inspection.
func testHub(t *testing.T, src *fakeSource, encs map[string]*fakeEncoder) (*Hub, func(name string) (Entry, error)) {
	t.Helper()
	if encs == nil {
		t.Fatal("encs map required")
	}

	specs := map[string]EncodeSpec{
		"intercom_browser": {Width: 640, Height: 1024, FPS: 12, Quality: 5},
		"intercom_esp":     {Width: 800, Height: 1280, FPS: 9, Quality: 6},
	}

	entryFor := func(name string) (Entry, error) {
		spec, ok := specs[name]
		if !ok {
			return Entry{}, profile.ErrUnknownProfile
		}
		return Entry{Spec: spec, Source: src}, nil
	}

	factory := func(label string, spec EncodeSpec) (encoderIface, error) {
		fe := newFakeEncoder(label, spec)
		encs[label] = fe
		return fe, nil
	}

	h, err := NewHub(HubOptions{
		EntryFor:         entryFor,
		Logger:           quietHubLogger(),
		EncoderFactory:   factory,
		SubscriberBuffer: 8,
	})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	return h, entryFor
}

// --- tests --------------------------------------------------------------------

func TestHub_SubscribeUnknownProfile(t *testing.T) {
	src := newFakeSource()
	encs := make(map[string]*fakeEncoder)
	h, _ := testHub(t, src, encs)
	defer h.Close()

	_, err := h.Subscribe("does-not-exist")
	if !errors.Is(err, profile.ErrUnknownProfile) {
		t.Errorf("err=%v, want profile.ErrUnknownProfile chain", err)
	}
}

func TestHub_FirstSubscribeStartsEncoderAndUpstream(t *testing.T) {
	src := newFakeSource()
	encs := make(map[string]*fakeEncoder)
	h, _ := testHub(t, src, encs)
	defer h.Close()

	sub, err := h.Subscribe("intercom_esp")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	enc := encs["intercom_esp"]
	if enc == nil {
		t.Fatal("encoder not built")
	}
	if !enc.started.Load() {
		t.Error("encoder Start not called")
	}
	if enc.spec.FPS != 9 {
		t.Errorf("encoder got fps=%d, want 9 (esp default)", enc.spec.FPS)
	}
	if src.subCount.Load() != 1 {
		t.Errorf("upstream subscriptions = %d, want 1", src.subCount.Load())
	}
}

func TestHub_TwoSubscribersOneEncoder(t *testing.T) {
	src := newFakeSource()
	encs := make(map[string]*fakeEncoder)
	h, _ := testHub(t, src, encs)
	defer h.Close()

	subA, _ := h.Subscribe("intercom_esp")
	subB, _ := h.Subscribe("intercom_esp")
	defer subA.Close()
	defer subB.Close()

	if got := len(encs); got != 1 {
		t.Errorf("encoders built = %d, want 1 (one Subscribe-pair, one profile)", got)
	}
	if src.subCount.Load() != 1 {
		t.Errorf("upstream subscriptions = %d, want 1 (shared)", src.subCount.Load())
	}
}

func TestHub_DifferentProfilesIndependent(t *testing.T) {
	src := newFakeSource()
	encs := make(map[string]*fakeEncoder)
	h, _ := testHub(t, src, encs)
	defer h.Close()

	a, _ := h.Subscribe("intercom_esp")
	b, _ := h.Subscribe("intercom_browser")
	defer a.Close()
	defer b.Close()

	if got := len(encs); got != 2 {
		t.Errorf("encoders built = %d, want 2 (one per profile)", got)
	}
	// Both profiles point to the same source in this test → 2 subscribes
	// to that source. In real life if EntryFor returned different
	// sources per profile, sub counts would split accordingly.
	if src.subCount.Load() != 2 {
		t.Errorf("upstream subscriptions = %d, want 2 (one per profile)", src.subCount.Load())
	}
}

func TestHub_JPEGDistributedToAllSubscribers(t *testing.T) {
	src := newFakeSource()
	encs := make(map[string]*fakeEncoder)
	h, _ := testHub(t, src, encs)
	defer h.Close()

	a, _ := h.Subscribe("intercom_esp")
	b, _ := h.Subscribe("intercom_esp")
	defer a.Close()
	defer b.Close()

	enc := encs["intercom_esp"]
	enc.emit([]byte{0xFF, 0xD8, 0x01, 0xFF, 0xD9})

	for _, sub := range []*Subscriber{a, b} {
		select {
		case frame, ok := <-sub.Frames():
			if !ok {
				t.Fatalf("sub %d: channel closed early", sub.ID())
			}
			if len(frame) != 5 || frame[0] != 0xFF || frame[1] != 0xD8 {
				t.Errorf("sub %d: unexpected frame %x", sub.ID(), frame)
			}
		case <-time.After(time.Second):
			t.Fatalf("sub %d: no frame within 1 s", sub.ID())
		}
	}
}

func TestHub_LastSubscribeClosesEncoderAndUpstream(t *testing.T) {
	src := newFakeSource()
	encs := make(map[string]*fakeEncoder)
	h, _ := testHub(t, src, encs)
	defer h.Close()

	sub, _ := h.Subscribe("intercom_esp")
	enc := encs["intercom_esp"]

	sub.Close()

	// Wait for the session goroutine to tear down.
	deadline := time.After(time.Second)
	for !enc.closed.Load() {
		select {
		case <-deadline:
			t.Fatal("encoder not closed after last subscriber left")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestHub_ResubscribeAfterDownToZeroRebuildsEncoder(t *testing.T) {
	src := newFakeSource()
	encs := make(map[string]*fakeEncoder)
	var builds atomic.Int64
	specs := map[string]EncodeSpec{
		"intercom_esp": {Width: 800, Height: 1280, FPS: 9, Quality: 6},
	}
	entryFor := func(name string) (Entry, error) {
		spec, ok := specs[name]
		if !ok {
			return Entry{}, profile.ErrUnknownProfile
		}
		return Entry{Spec: spec, Source: src}, nil
	}
	factory := func(label string, spec EncodeSpec) (encoderIface, error) {
		builds.Add(1)
		fe := newFakeEncoder(label, spec)
		encs[label] = fe
		return fe, nil
	}
	h, err := NewHub(HubOptions{
		EntryFor:       entryFor,
		Logger:         quietHubLogger(),
		EncoderFactory: factory,
	})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	defer h.Close()

	a, _ := h.Subscribe("intercom_esp")
	a.Close()
	time.Sleep(50 * time.Millisecond) // let teardown complete

	b, _ := h.Subscribe("intercom_esp")
	defer b.Close()

	if got := builds.Load(); got != 2 {
		t.Errorf("encoder builds = %d, want 2 (one per lifetime)", got)
	}
}

func TestHub_EncoderEndClosesAllSubscriberChannels(t *testing.T) {
	src := newFakeSource()
	encs := make(map[string]*fakeEncoder)
	h, _ := testHub(t, src, encs)
	defer h.Close()

	a, _ := h.Subscribe("intercom_esp")
	b, _ := h.Subscribe("intercom_esp")
	enc := encs["intercom_esp"]

	// Simulate encoder dying.
	_ = enc.Close()

	for _, sub := range []*Subscriber{a, b} {
		select {
		case _, ok := <-sub.Frames():
			if ok {
				// Drain anything pending.
				continue
			}
		case <-time.After(time.Second):
			t.Fatalf("sub %d channel not closed after encoder end", sub.ID())
		}
	}
}

func TestHub_UpstreamEndTearsDownSession(t *testing.T) {
	src := newFakeSource()
	encs := make(map[string]*fakeEncoder)
	h, _ := testHub(t, src, encs)
	defer h.Close()

	sub, _ := h.Subscribe("intercom_esp")
	defer sub.Close()

	// Find the upstream subscriber and close it (= upstream end).
	src.mu.Lock()
	upstream := src.subs[0]
	src.mu.Unlock()
	upstream.Close()

	// Encoder should be closed once the forwarder unwinds.
	enc := encs["intercom_esp"]
	deadline := time.After(time.Second)
	for !enc.closed.Load() {
		select {
		case <-deadline:
			t.Fatal("encoder not closed after upstream end")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestHub_CloseShutsDownAllSessions(t *testing.T) {
	src := newFakeSource()
	encs := make(map[string]*fakeEncoder)
	h, _ := testHub(t, src, encs)

	subA, _ := h.Subscribe("intercom_esp")
	subB, _ := h.Subscribe("intercom_browser")
	_ = subA
	_ = subB

	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for name, enc := range encs {
		if !enc.closed.Load() {
			t.Errorf("encoder %q not closed after hub.Close", name)
		}
	}
}

func TestHub_EntryForErrorBubblesUp(t *testing.T) {
	wantErr := errors.New("synthetic entryFor failure")
	entryFor := func(name string) (Entry, error) {
		return Entry{}, wantErr
	}
	h, err := NewHub(HubOptions{
		EntryFor: entryFor,
		Logger:   quietHubLogger(),
	})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	defer h.Close()

	_, err = h.Subscribe("anything")
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want chain containing wantErr", err)
	}
}

func TestHub_AUForwardedToEncoderInput(t *testing.T) {
	src := newFakeSource()
	encs := make(map[string]*fakeEncoder)
	h, _ := testHub(t, src, encs)
	defer h.Close()

	sub, _ := h.Subscribe("intercom_esp")
	defer sub.Close()
	enc := encs["intercom_esp"]

	au := source.AccessUnit{NALUs: [][]byte{{0x65, 0x01}}, PTS: 12345}
	src.broadcast(au)

	select {
	case got := <-enc.in:
		if got.PTS != 12345 {
			t.Errorf("encoder got PTS=%d, want 12345", got.PTS)
		}
	case <-time.After(time.Second):
		t.Fatal("AU never reached encoder")
	}
}

func TestHub_Subscriber_CloseIdempotent(t *testing.T) {
	src := newFakeSource()
	encs := make(map[string]*fakeEncoder)
	h, _ := testHub(t, src, encs)
	defer h.Close()

	sub, _ := h.Subscribe("intercom_esp")
	sub.Close()
	sub.Close()
	sub.Close()
}

func TestHub_SubscriberHasUniqueIDs(t *testing.T) {
	src := newFakeSource()
	encs := make(map[string]*fakeEncoder)
	h, _ := testHub(t, src, encs)
	defer h.Close()

	const n = 5
	subs := make([]*Subscriber, n)
	for i := 0; i < n; i++ {
		s, err := h.Subscribe("intercom_esp")
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
			t.Errorf("duplicate ID %d", s.ID())
		}
		seen[s.ID()] = true
	}
}

func TestHub_NilEntryForRejected(t *testing.T) {
	_, err := NewHub(HubOptions{
		EntryFor: nil,
		Logger:   quietHubLogger(),
	})
	if err == nil {
		t.Fatal("expected error for nil EntryFor")
	}
}

// --- S6-10: spec-change retiring -----------------------------------------------

// TestHub_SpecChangeRetiresOldSession is the S6-10 contract. After a
// PUT /api/profiles changes a profile's EncodeSpec, the NEXT
// Subscribe must spin up a fresh encoder with the new spec — not
// join the long-lived old session and silently keep the old fps.
//
// Construction: a resolver whose returned spec we can mutate.
// Sequence:
//  1. Subscribe → spawns encoder #1 with spec A (fps 12).
//  2. Mutate the spec to B (fps 30) while session #1 is still alive
//     (subscriber #1 didn't disconnect — this models a long-lived
//     ESP connection + a freshly-arriving parallel client).
//  3. Subscribe again → MUST observe the spec change and spawn
//     encoder #2 with spec B.
//  4. Old subscriber keeps streaming on the old encoder.
//  5. The hub.sessions map now points to the NEW session for that name.
func TestHub_SpecChangeRetiresOldSession(t *testing.T) {
	src := newFakeSource()
	encs := make(map[string]*fakeEncoder)

	// Mutable spec source — the resolver reads it on every entry-for
	// call. This is the test-side analogue of "PUT updated the
	// profile registry".
	var (
		specMu      sync.Mutex
		currentSpec = EncodeSpec{Width: 800, Height: 1280, FPS: 12, Quality: 6}
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

	// Each fake encoder gets a unique tag we can assert on. The
	// encoder factory pushes them into a slice (so we can prove
	// "exactly 2 encoders existed" rather than reusing one).
	var encoders []*fakeEncoder
	factory := func(label string, spec EncodeSpec) (encoderIface, error) {
		fe := newFakeEncoder(label, spec)
		encoders = append(encoders, fe)
		encs[label] = fe
		return fe, nil
	}

	h, err := NewHub(HubOptions{
		EntryFor: func(name string) (Entry, error) {
			if name != "mjpeg_bal" {
				return Entry{}, profile.ErrUnknownProfile
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

	// Step 1: first subscriber.
	sub1, err := h.Subscribe("mjpeg_bal")
	if err != nil {
		t.Fatalf("Subscribe #1: %v", err)
	}
	if len(encoders) != 1 {
		t.Fatalf("after first Subscribe: encoder count = %d, want 1", len(encoders))
	}
	if got := encoders[0].spec.FPS; got != 12 {
		t.Errorf("encoder #1 spawned with FPS=%d, want 12", got)
	}

	// Step 2: mutate spec (simulates PUT /api/profiles/mjpeg_bal fps=30).
	setSpec(EncodeSpec{Width: 800, Height: 1280, FPS: 30, Quality: 4})

	// Step 3: second subscriber arrives AFTER the spec change.
	// Pre-S6-10 this would have joined encoder #1 (fps=12). Now it
	// must trigger a fresh encoder.
	sub2, err := h.Subscribe("mjpeg_bal")
	if err != nil {
		t.Fatalf("Subscribe #2: %v", err)
	}
	if len(encoders) != 2 {
		t.Fatalf("after spec-change Subscribe: encoder count = %d, want 2", len(encoders))
	}
	if got := encoders[1].spec.FPS; got != 30 {
		t.Errorf("encoder #2 spawned with FPS=%d, want 30 (the new spec)", got)
	}
	if got := encoders[1].spec.Quality; got != 4 {
		t.Errorf("encoder #2 spawned with Quality=%d, want 4", got)
	}

	// Step 4: old subscriber still receives frames on encoder #1.
	encoders[0].emit([]byte{0xFF, 0xD8, 0x12, 0xFF, 0xD9})
	select {
	case <-sub1.Frames():
		// good
	case <-time.After(time.Second):
		t.Error("sub1 did not receive a frame from the OLD encoder; existing viewers must keep streaming on the retired session")
	}

	// Step 5: new subscriber receives frames on encoder #2.
	encoders[1].emit([]byte{0xFF, 0xD8, 0x30, 0xFF, 0xD9})
	select {
	case <-sub2.Frames():
		// good
	case <-time.After(time.Second):
		t.Error("sub2 did not receive a frame from the NEW encoder")
	}

	// Step 6: a THIRD subscriber should also land on encoder #2
	// (the new session is the one in the map now).
	sub3, err := h.Subscribe("mjpeg_bal")
	if err != nil {
		t.Fatalf("Subscribe #3: %v", err)
	}
	if len(encoders) != 2 {
		t.Errorf("after Subscribe #3: encoder count = %d, want 2 (joined new session, no third encoder)", len(encoders))
	}
	// Cleanup.
	sub1.Close()
	sub2.Close()
	sub3.Close()
}

// TestHub_SpecUnchangedJoinsExistingSession is the inverse: if the
// EncodeSpec hasn't changed since the session started, a new
// Subscribe must JOIN the existing encoder — not spawn a duplicate.
// That's the fan-out invariant.
func TestHub_SpecUnchangedJoinsExistingSession(t *testing.T) {
	src := newFakeSource()
	encs := make(map[string]*fakeEncoder)
	h, _ := testHub(t, src, encs)
	defer h.Close()

	var subs []*Subscriber
	for i := 0; i < 5; i++ {
		s, err := h.Subscribe("intercom_esp")
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
		t.Errorf("encoder count = %d, want 1 (fan-out invariant broken — every Subscribe spawned a new encoder)", len(encs))
	}
}
