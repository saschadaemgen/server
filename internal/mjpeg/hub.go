package mjpeg

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"carvilon.local/stream/internal/droplog"
	"carvilon.local/stream/internal/hub"
	"carvilon.local/stream/internal/source"
)

// ErrUnknownProfile is returned by [Hub.Subscribe] when no profile with
// the requested name is registered. Maps cleanly to a 404 in the HTTP
// handler.
var ErrUnknownProfile = errors.New("mjpeg: unknown profile")

// SourceHub is the interface [Hub] needs from the upstream H.264 bus
// — just enough to subscribe and let the subscriber close itself.
// Matched by [hub.Hub]. The interface (rather than a concrete
// reference) lets the MJPEG hub be tested with a fake.
type SourceHub interface {
	Subscribe() (sourceSubscriber, error)
}

// sourceSubscriber abstracts the upstream subscriber. Matched by
// [hub.Subscriber] via the adapter in [NewHub].
type sourceSubscriber interface {
	Frames() <-chan source.AccessUnit
	Close()
}

// encoderIface is the minimal contract [Hub] needs from an encoder.
// The concrete [Encoder] satisfies it; tests inject a fake via
// HubOptions.EncoderFactory so the MJPEG hub can be exercised without
// spawning ffmpeg.
type encoderIface interface {
	Start() error
	Input() chan<- source.AccessUnit
	JPEGs() <-chan []byte
	Close() error
}

// compile-time assertion
var _ encoderIface = (*Encoder)(nil)

// EncoderFactory builds an encoder for a profile. The default factory
// (when HubOptions.EncoderFactory is nil) returns a real ffmpeg-backed
// [Encoder].
type EncoderFactory func(Profile) (encoderIface, error)

// defaultEncoderFactory wires up the real ffmpeg-backed encoder. Used
// when HubOptions.EncoderFactory is nil.
func defaultEncoderFactory(opts HubOptions) EncoderFactory {
	return func(prof Profile) (encoderIface, error) {
		return NewEncoder(EncoderOptions{
			Profile:    prof,
			FFmpegPath: opts.FFmpegPath,
			Logger:     opts.Logger,
			InputBuf:   opts.EncoderInputBuf,
			OutputBuf:  opts.EncoderOutputBuf,
		})
	}
}

// Hub fans out a single ffmpeg encoder's output to many MJPEG HTTP
// viewers. ONE encoder per profile; if a profile has no viewers, no
// encoder runs and no upstream subscription exists.
//
// Architecture (per profile, lazy-built on first Subscribe):
//
//	stream-hub Subscriber  ──►  Encoder (ffmpeg subprocess)  ──►  JPEG fan-out
//	                                                                  │
//	                                                       per-viewer Subscribers
//
// Lifecycle:
//   - First Subscribe for profile X: build upstream subscriber, build
//     encoder, spawn forwarder + run goroutines.
//   - Further Subscribe for X: just adds another subscriber to the
//     running session.
//   - Last viewer for X leaves: encoder.Close(), upstream subscriber
//     Close(), session removed from the map.
//   - Sessions for different profiles are independent.
//
// Concurrent Subscribe / Close from many HTTP handlers is safe: a
// session's subscriber list is owned by the session's `run` goroutine
// and mutated only via channels; the hub-level map is mutex-protected.
type Hub struct {
	src          SourceHub
	profiles     map[string]Profile
	logger       *log.Logger
	subBufSize   int
	encFactory   EncoderFactory

	mu       sync.Mutex
	sessions map[string]*session

	closed    chan struct{}
	closeOnce sync.Once
}

// HubOptions configures a [Hub].
type HubOptions struct {
	// StreamHub is the H.264 source bus. Required (unless SourceHub
	// is set, e.g. by tests with a fake upstream).
	StreamHub *hub.Hub

	// SourceHub overrides StreamHub with a custom upstream. Useful for
	// tests; in production, leave nil and set StreamHub.
	SourceHub SourceHub

	// Profiles are the named encode targets the hub will accept on
	// Subscribe. ErrUnknownProfile is returned for any name not in
	// this set.
	Profiles []Profile

	// FFmpegPath defaults to "ffmpeg".
	FFmpegPath string

	// Logger receives diagnostic output.
	Logger *log.Logger

	// SubscriberBuffer is the per-viewer JPEG channel depth. Default 30
	// (≈3.3 s at 9 fps).
	SubscriberBuffer int

	// EncoderInputBuf / EncoderOutputBuf — buffers on the encoder side.
	// Defaults preserved if zero.
	EncoderInputBuf  int
	EncoderOutputBuf int

	// EncoderFactory overrides how encoders are built. Default: real
	// ffmpeg-subprocess encoder. Tests inject a fake here.
	EncoderFactory EncoderFactory
}

// NewHub validates options and returns a ready-to-use Hub. No encoder
// is spawned until the first [Hub.Subscribe].
func NewHub(opts HubOptions) (*Hub, error) {
	if opts.SourceHub == nil && opts.StreamHub == nil {
		return nil, errors.New("mjpeg: StreamHub (or SourceHub for tests) is required")
	}
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	if opts.FFmpegPath == "" {
		opts.FFmpegPath = "ffmpeg"
	}
	if opts.SubscriberBuffer <= 0 {
		opts.SubscriberBuffer = 30
	}

	pm := make(map[string]Profile, len(opts.Profiles))
	for _, p := range opts.Profiles {
		if err := p.Validate(); err != nil {
			return nil, err
		}
		if _, dup := pm[p.Name]; dup {
			return nil, fmt.Errorf("mjpeg: duplicate profile name %q", p.Name)
		}
		pm[p.Name] = p
	}

	src := opts.SourceHub
	if src == nil {
		src = &streamHubAdapter{h: opts.StreamHub}
	}

	encFactory := opts.EncoderFactory
	if encFactory == nil {
		encFactory = defaultEncoderFactory(opts)
	}

	return &Hub{
		src:        src,
		profiles:   pm,
		logger:     opts.Logger,
		subBufSize: opts.SubscriberBuffer,
		encFactory: encFactory,
		sessions:   make(map[string]*session),
		closed:     make(chan struct{}),
	}, nil
}

// ProfileNames returns the set of registered profile names, sorted
// alphabetically. Handy for /healthz-style introspection.
func (h *Hub) ProfileNames() []string {
	names := make([]string, 0, len(h.profiles))
	for n := range h.profiles {
		names = append(names, n)
	}
	// Simple bubble: tiny N, not worth the sort import dance.
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	return names
}

// Subscribe attaches a new MJPEG viewer to the named profile. Returns
// [ErrUnknownProfile] for an unregistered name, or any error bubbling
// up from the encoder / upstream subscription on first-viewer startup.
func (h *Hub) Subscribe(profileName string) (*Subscriber, error) {
	prof, ok := h.profiles[profileName]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownProfile, profileName)
	}

	h.mu.Lock()
	if isClosed(h.closed) {
		h.mu.Unlock()
		return nil, errors.New("mjpeg: hub closed")
	}

	sess := h.sessions[profileName]
	if sess == nil {
		// First viewer for this profile — build everything.
		newSess, err := h.startSessionLocked(prof)
		if err != nil {
			h.mu.Unlock()
			return nil, err
		}
		sess = newSess
		h.sessions[profileName] = sess
	}
	h.mu.Unlock()

	// Ask the session to register us. This goes via a channel so the
	// session's run goroutine remains the sole owner of the subs map.
	resp := make(chan addSubResp, 1)
	select {
	case sess.addCh <- addSubReq{resp: resp}:
	case <-sess.done:
		// Session died between the map lookup and our send.
		return nil, errors.New("mjpeg: session ended during subscribe")
	}
	r := <-resp
	return r.sub, r.err
}

// Close stops every active session and waits for cleanup. Idempotent.
func (h *Hub) Close() error {
	h.closeOnce.Do(func() {
		close(h.closed)
	})
	h.mu.Lock()
	sessions := make([]*session, 0, len(h.sessions))
	for _, s := range h.sessions {
		sessions = append(sessions, s)
	}
	h.mu.Unlock()

	for _, s := range sessions {
		s.cancel()
		<-s.done
	}
	return nil
}

// startSessionLocked must be called with h.mu held. It subscribes to
// the upstream H.264 hub, spawns an encoder, and starts the session
// goroutines.
func (h *Hub) startSessionLocked(prof Profile) (*session, error) {
	upstream, err := h.src.Subscribe()
	if err != nil {
		return nil, fmt.Errorf("mjpeg: upstream subscribe: %w", err)
	}

	enc, err := h.encFactory(prof)
	if err != nil {
		upstream.Close()
		return nil, err
	}
	if err := enc.Start(); err != nil {
		upstream.Close()
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	sess := &session{
		profile:    prof,
		hub:        h,
		encoder:    enc,
		upstream:   upstream,
		addCh:      make(chan addSubReq),
		unsubCh:    make(chan uint64),
		ctx:        ctx,
		cancel:     cancel,
		done:       make(chan struct{}),
		subBufSize: h.subBufSize,
	}
	go sess.runForwarder()
	go sess.run()

	h.logger.Printf("mjpeg: session %q started", prof.Name)
	return sess, nil
}

// removeSession is called by a session as it exits, so the hub map
// drops the now-dead pointer. Safe to call concurrently with Subscribe.
func (h *Hub) removeSession(name string, want *session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if got, ok := h.sessions[name]; ok && got == want {
		delete(h.sessions, name)
	}
}

// session is the per-profile state. One run goroutine owns the
// subscriber list and routes JPEG frames; one forwarder goroutine pumps
// H.264 AUs from upstream into the encoder's input channel.
type session struct {
	profile Profile
	hub     *Hub

	encoder  encoderIface
	upstream sourceSubscriber

	addCh   chan addSubReq
	unsubCh chan uint64

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	subBufSize int
}

type addSubReq struct {
	resp chan addSubResp
}

type addSubResp struct {
	sub *Subscriber
	err error
}

// runForwarder pumps AUs from the upstream subscriber into the encoder.
// Non-blocking send: if ffmpeg can't keep up the AU is dropped so the
// upstream hub (and other subscribers attached to it) keeps flowing.
func (s *session) runForwarder() {
	dc := &droplog.Counter{
		Logger: s.hub.logger,
		Label:  fmt.Sprintf("mjpeg: session %q encoder input", s.profile.Name),
	}
	for {
		select {
		case <-s.ctx.Done():
			return
		case au, ok := <-s.upstream.Frames():
			if !ok {
				// Upstream ended. Tear the session down.
				s.cancel()
				return
			}
			select {
			case s.encoder.Input() <- au:
			default:
				dc.Record(errors.New("encoder input channel full"))
			}
		}
	}
}

// run is the per-session distribution loop. Owns the subscriber map.
func (s *session) run() {
	defer close(s.done)
	defer s.teardown()

	subscribers := make(map[uint64]*Subscriber)
	var nextID uint64

	for {
		select {
		case <-s.ctx.Done():
			for _, sub := range subscribers {
				close(sub.frames)
			}
			return

		case req := <-s.addCh:
			nextID++
			sub := &Subscriber{
				id:     nextID,
				frames: make(chan []byte, s.subBufSize),
				drops: &droplog.Counter{
					Logger: s.hub.logger,
					Label:  fmt.Sprintf("mjpeg: session %q viewer %d", s.profile.Name, nextID),
				},
				session: s,
			}
			subscribers[nextID] = sub
			s.hub.logger.Printf("mjpeg: session %q viewer %d joined (total=%d)",
				s.profile.Name, sub.id, len(subscribers))
			req.resp <- addSubResp{sub: sub}

		case id := <-s.unsubCh:
			sub, ok := subscribers[id]
			if !ok {
				continue
			}
			delete(subscribers, id)
			close(sub.frames)
			s.hub.logger.Printf("mjpeg: session %q viewer %d left (total=%d)",
				s.profile.Name, id, len(subscribers))
			if len(subscribers) == 0 {
				s.hub.logger.Printf("mjpeg: session %q last viewer left", s.profile.Name)
				return
			}

		case frame, ok := <-s.encoder.JPEGs():
			if !ok {
				s.hub.logger.Printf("mjpeg: session %q encoder ended", s.profile.Name)
				for _, sub := range subscribers {
					close(sub.frames)
				}
				return
			}
			for _, sub := range subscribers {
				select {
				case sub.frames <- frame:
				default:
					sub.drops.Record(errors.New("viewer frames channel full"))
				}
			}
		}
	}
}

// teardown is called from run() on exit (any reason). Closes the
// encoder + upstream subscriber and detaches from the hub map.
func (s *session) teardown() {
	s.cancel()
	_ = s.encoder.Close()
	s.upstream.Close()
	s.hub.removeSession(s.profile.Name, s)
	s.hub.logger.Printf("mjpeg: session %q stopped", s.profile.Name)
}

// Subscriber represents one connected MJPEG HTTP viewer. Obtain via
// [Hub.Subscribe]; release with [Subscriber.Close] when the HTTP
// response is over (client disconnect, error, server shutdown).
type Subscriber struct {
	id      uint64
	frames  chan []byte
	drops   *droplog.Counter
	session *session
	once    sync.Once
}

// ID returns the unique-per-session viewer id (handy for log correlation).
func (s *Subscriber) ID() uint64 { return s.id }

// Frames returns the read-only stream of JPEG frames for this viewer.
// Channel is closed by the hub on Subscriber.Close, session end, or
// hub shutdown.
func (s *Subscriber) Frames() <-chan []byte { return s.frames }

// Close detaches the viewer from its session. Idempotent.
func (s *Subscriber) Close() {
	s.once.Do(func() {
		select {
		case s.session.unsubCh <- s.id:
		case <-s.session.done:
		}
	})
}

// --- upstream-Hub adapter ---------------------------------------------------

// streamHubAdapter exposes [hub.Hub]'s narrow Subscribe contract as the
// generic [SourceHub] interface mjpeg.Hub depends on. The two-level
// indirection makes the mjpeg.Hub testable without spinning up a real
// stream.Hub.
type streamHubAdapter struct{ h *hub.Hub }

func (a *streamHubAdapter) Subscribe() (sourceSubscriber, error) {
	sub, err := a.h.Subscribe()
	if err != nil {
		return nil, err
	}
	return &streamSubAdapter{s: sub}, nil
}

type streamSubAdapter struct{ s *hub.Subscriber }

func (a *streamSubAdapter) Frames() <-chan source.AccessUnit { return a.s.Frames() }
func (a *streamSubAdapter) Close()                           { a.s.Close() }

// isClosed returns true if ch has been closed. Cheap-and-correct because
// we use the channel only as a one-shot signal (see [Hub.closed]).
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
