package mjpeg

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"carvilon.local/stream/internal/droplog"
	"carvilon.local/stream/internal/source"
)

// SourceHub is the interface [Hub] needs from the upstream H.264 bus —
// just enough to subscribe and let the subscriber close itself. Matched
// by *hub.Hub via an adapter in the server layer, and by test fakes.
type SourceHub interface {
	Subscribe() (SourceSubscriber, error)
}

// SourceSubscriber abstracts the upstream subscriber returned by a
// SourceHub.Subscribe call.
type SourceSubscriber interface {
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

// EncoderFactory builds an encoder for a given label+spec. The default
// factory (when HubOptions.EncoderFactory is nil) returns a real
// ffmpeg-backed [Encoder].
type EncoderFactory func(label string, spec EncodeSpec) (encoderIface, error)

// Entry is the resolved configuration for one MJPEG profile: which
// H.264 hub to source from, and how to encode the output.
//
// The hub's [HubOptions.EntryFor] function returns one of these per
// profile name on first Subscribe. Multiple profile names pointing to
// the same Source (same camera/quality) share a single upstream pull,
// because EntryFor returns the same SourceHub identity.
type Entry struct {
	Spec   EncodeSpec
	Source SourceHub
}

// defaultEncoderFactory wires up the real ffmpeg-backed encoder.
func defaultEncoderFactory(opts HubOptions) EncoderFactory {
	return func(label string, spec EncodeSpec) (encoderIface, error) {
		return NewEncoder(EncoderOptions{
			Label:      label,
			Spec:       spec,
			FFmpegPath: opts.FFmpegPath,
			Logger:     opts.Logger,
			InputBuf:   opts.EncoderInputBuf,
			OutputBuf:  opts.EncoderOutputBuf,
		})
	}
}

// Hub fans out a single ffmpeg encoder's output to many MJPEG HTTP
// viewers. ONE encoder per profile name; if a profile has no viewers,
// no encoder runs and no upstream subscription exists.
//
// Architecture (per profile, lazy-built on first Subscribe):
//
//	stream-hub Subscriber  ──►  Encoder (ffmpeg subprocess)  ──►  JPEG fan-out
//	                                                                  │
//	                                                       per-viewer Subscribers
//
// Lifecycle:
//   - First Subscribe for profile X: call EntryFor(X), subscribe to the
//     returned Source, build an encoder with the returned Spec, start
//     it, spawn forwarder + run goroutines.
//   - Further Subscribe for X: just adds another subscriber to the
//     running session.
//   - Last viewer for X leaves: encoder.Close(), upstream subscriber
//     Close(), session removed from the map.
//   - Sessions for different profiles are independent.
//   - Two profiles whose EntryFor returns the same Source identity
//     share a single upstream pull (the source-registry layer handles
//     that — the mjpeg.Hub just observes whatever Source identity it
//     gets).
type Hub struct {
	entryFor   func(name string) (Entry, error)
	logger     *log.Logger
	subBufSize int
	encFactory EncoderFactory

	mu       sync.Mutex
	sessions map[string]*session

	closed    chan struct{}
	closeOnce sync.Once
}

// HubOptions configures a [Hub].
type HubOptions struct {
	// EntryFor resolves a profile name to its [Entry] (encode spec +
	// source hub). The server typically implements this by looking
	// the profile up in a [profile.Registry], validating it's an
	// MJPEG-usage profile, and asking a source registry for the
	// camera's hub. Required.
	EntryFor func(name string) (Entry, error)

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
	if opts.EntryFor == nil {
		return nil, errors.New("mjpeg: EntryFor is required")
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

	encFactory := opts.EncoderFactory
	if encFactory == nil {
		encFactory = defaultEncoderFactory(opts)
	}

	return &Hub{
		entryFor:   opts.EntryFor,
		logger:     opts.Logger,
		subBufSize: opts.SubscriberBuffer,
		encFactory: encFactory,
		sessions:   make(map[string]*session),
		closed:     make(chan struct{}),
	}, nil
}

// Subscribe attaches a new MJPEG viewer to the named profile. The
// underlying EntryFor decides whether the name is known and resolves
// it to an Entry — propagate its error verbatim (typically
// [profile.ErrUnknownProfile] for unknown names, which the HTTP layer
// maps to 404).
func (h *Hub) Subscribe(name string) (*Subscriber, error) {
	h.mu.Lock()
	if isClosed(h.closed) {
		h.mu.Unlock()
		return nil, errors.New("mjpeg: hub closed")
	}

	sess := h.sessions[name]
	if sess == nil {
		newSess, err := h.startSessionLocked(name)
		if err != nil {
			h.mu.Unlock()
			return nil, err
		}
		sess = newSess
		h.sessions[name] = sess
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

// startSessionLocked must be called with h.mu held. It resolves the
// profile name to its Entry, subscribes to the upstream source, spawns
// an encoder, and starts the session goroutines.
func (h *Hub) startSessionLocked(name string) (*session, error) {
	entry, err := h.entryFor(name)
	if err != nil {
		return nil, err
	}
	if entry.Source == nil {
		return nil, fmt.Errorf("mjpeg: profile %q has no Source", name)
	}
	if err := entry.Spec.Validate(); err != nil {
		return nil, fmt.Errorf("mjpeg: profile %q: %w", name, err)
	}

	upstream, err := entry.Source.Subscribe()
	if err != nil {
		return nil, fmt.Errorf("mjpeg: upstream subscribe: %w", err)
	}

	enc, err := h.encFactory(name, entry.Spec)
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
		name:       name,
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

	h.logger.Printf("mjpeg: session %q started", name)
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
	name string
	hub  *Hub

	encoder  encoderIface
	upstream SourceSubscriber

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
		Label:  fmt.Sprintf("mjpeg: session %q encoder input", s.name),
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
					Label:  fmt.Sprintf("mjpeg: session %q viewer %d", s.name, nextID),
				},
				session: s,
			}
			subscribers[nextID] = sub
			s.hub.logger.Printf("mjpeg: session %q viewer %d joined (total=%d)",
				s.name, sub.id, len(subscribers))
			req.resp <- addSubResp{sub: sub}

		case id := <-s.unsubCh:
			sub, ok := subscribers[id]
			if !ok {
				continue
			}
			delete(subscribers, id)
			close(sub.frames)
			s.hub.logger.Printf("mjpeg: session %q viewer %d left (total=%d)",
				s.name, id, len(subscribers))
			if len(subscribers) == 0 {
				s.hub.logger.Printf("mjpeg: session %q last viewer left", s.name)
				return
			}

		case frame, ok := <-s.encoder.JPEGs():
			if !ok {
				s.hub.logger.Printf("mjpeg: session %q encoder ended", s.name)
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
	s.hub.removeSession(s.name, s)
	s.hub.logger.Printf("mjpeg: session %q stopped", s.name)
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
