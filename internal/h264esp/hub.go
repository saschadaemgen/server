package h264esp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"carvilon.local/stream/internal/droplog"
	"carvilon.local/stream/internal/source"
)

// SourceHub is the contract this hub needs from the upstream H.264
// bus — just enough to subscribe and let the subscriber close itself.
// Mirrored by *hub.Hub via an adapter in the server layer (the same
// adapter used by internal/mjpeg, so a single camera pull feeds both
// transcoders).
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
// HubOptions.EncoderFactory so the hub can be exercised without
// spawning ffmpeg.
type encoderIface interface {
	Start() error
	Input() chan<- source.AccessUnit
	AUs() <-chan []byte
	Close() error
}

// compile-time assertion
var _ encoderIface = (*Encoder)(nil)

// EncoderFactory builds an encoder for a given label+spec. The default
// factory (when HubOptions.EncoderFactory is nil) returns a real
// ffmpeg-backed [Encoder].
type EncoderFactory func(label string, spec EncodeSpec) (encoderIface, error)

// Entry is the resolved configuration for one h264_cbp profile.
//
// Identical shape to mjpeg.Entry but kept distinct so the two
// codepaths can diverge if needed (e.g. h264_cbp might one day want
// a different Source-side adapter for latency-sensitive routing).
type Entry struct {
	Spec   EncodeSpec
	Source SourceHub
}

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

// Hub fans out a single ffmpeg encoder's output to many H.264 HTTP
// viewers. Lifecycle is bedarfsgesteuert: ONE encoder per profile,
// spawned lazily on first Subscribe and torn down on last
// Unsubscribe.
//
// Two profiles whose EntryFor returns the same Source identity share
// a single upstream camera pull — the source-registry layer handles
// that; this hub just observes whatever Source identity it gets.
type Hub struct {
	entryFor   func(name string) (Entry, error)
	logger     *log.Logger
	subBufSize int
	encFactory EncoderFactory

	// S6-04 source-measurement hooks.
	onSourceAU     func(profileName string)
	onSessionStart func(profileName string)

	mu       sync.Mutex
	sessions map[string]*session

	closed    chan struct{}
	closeOnce sync.Once
}

// HubOptions configures a [Hub].
type HubOptions struct {
	// EntryFor resolves a profile name to its [Entry] (encode spec +
	// source hub). Required.
	EntryFor func(name string) (Entry, error)

	// FFmpegPath defaults to "ffmpeg".
	FFmpegPath string

	// Logger receives diagnostic output.
	Logger *log.Logger

	// SubscriberBuffer is the per-viewer AU channel depth. Default 2
	// (S6-07: was 30 — same latency-capacity issue as the MJPEG hub.
	// At 15 fps the old default added up to 2 s of capacity; with
	// drop-on-overflow we keep the fan-out resilient to a wedged
	// client but no longer build up perceptible lag).
	SubscriberBuffer int

	// EncoderInputBuf / EncoderOutputBuf — buffers on the encoder side.
	EncoderInputBuf  int
	EncoderOutputBuf int

	// EncoderFactory overrides how encoders are built. Default: real
	// ffmpeg-subprocess encoder. Tests inject a fake.
	EncoderFactory EncoderFactory

	// OnSourceAU / OnSessionStart (S6-04, optional): see
	// internal/mjpeg.HubOptions for the rationale. Same hooks, same
	// semantics — the per-codec hub doesn't import stats directly.
	OnSourceAU     func(profileName string)
	OnSessionStart func(profileName string)
}

// NewHub validates options and returns a ready-to-use Hub. No encoder
// is spawned until the first [Hub.Subscribe].
func NewHub(opts HubOptions) (*Hub, error) {
	if opts.EntryFor == nil {
		return nil, errors.New("h264esp: EntryFor is required")
	}
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	if opts.FFmpegPath == "" {
		opts.FFmpegPath = "ffmpeg"
	}
	if opts.SubscriberBuffer <= 0 {
		// S6-07: low-latency default — see HubOptions doc and the
		// mirror change in internal/mjpeg.HubOptions.
		opts.SubscriberBuffer = 2
	}

	encFactory := opts.EncoderFactory
	if encFactory == nil {
		encFactory = defaultEncoderFactory(opts)
	}

	return &Hub{
		entryFor:       opts.EntryFor,
		logger:         opts.Logger,
		subBufSize:     opts.SubscriberBuffer,
		encFactory:     encFactory,
		onSourceAU:     opts.OnSourceAU,
		onSessionStart: opts.OnSessionStart,
		sessions:       make(map[string]*session),
		closed:         make(chan struct{}),
	}, nil
}

// Subscribe attaches a new H.264 viewer to the named profile. The
// underlying EntryFor decides whether the name is known and resolves
// it to an Entry — propagate its error verbatim (typically
// [profile.ErrUnknownProfile] for unknown names, which the HTTP layer
// maps to 404).
func (h *Hub) Subscribe(name string) (*Subscriber, error) {
	h.mu.Lock()
	if isClosed(h.closed) {
		h.mu.Unlock()
		return nil, errors.New("h264esp: hub closed")
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

	resp := make(chan addSubResp, 1)
	select {
	case sess.addCh <- addSubReq{resp: resp}:
	case <-sess.done:
		return nil, errors.New("h264esp: session ended during subscribe")
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

func (h *Hub) startSessionLocked(name string) (*session, error) {
	// S6-04: notify before the encoder spawns so the source-counter
	// gets reset before the forwarder's first frame lands.
	if h.onSessionStart != nil {
		h.onSessionStart(name)
	}
	entry, err := h.entryFor(name)
	if err != nil {
		return nil, err
	}
	if entry.Source == nil {
		return nil, fmt.Errorf("h264esp: profile %q has no Source", name)
	}
	if err := entry.Spec.Validate(); err != nil {
		return nil, fmt.Errorf("h264esp: profile %q: %w", name, err)
	}

	upstream, err := entry.Source.Subscribe()
	if err != nil {
		return nil, fmt.Errorf("h264esp: upstream subscribe: %w", err)
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

	h.logger.Printf("h264esp: session %q started", name)
	return sess, nil
}

func (h *Hub) removeSession(name string, want *session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if got, ok := h.sessions[name]; ok && got == want {
		delete(h.sessions, name)
	}
}

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
		Label:  fmt.Sprintf("h264esp: session %q encoder input", s.name),
	}
	for {
		select {
		case <-s.ctx.Done():
			return
		case au, ok := <-s.upstream.Frames():
			if !ok {
				s.cancel()
				return
			}
			// S6-04: count the camera-side AU even if the encoder
			// drops it — source_fps measures what the camera is
			// delivering, not what the encoder makes of it.
			if s.hub.onSourceAU != nil {
				s.hub.onSourceAU(s.name)
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
					Label:  fmt.Sprintf("h264esp: session %q viewer %d", s.name, nextID),
				},
				session: s,
			}
			subscribers[nextID] = sub
			s.hub.logger.Printf("h264esp: session %q viewer %d joined (total=%d)",
				s.name, sub.id, len(subscribers))
			req.resp <- addSubResp{sub: sub}

		case id := <-s.unsubCh:
			sub, ok := subscribers[id]
			if !ok {
				continue
			}
			delete(subscribers, id)
			close(sub.frames)
			s.hub.logger.Printf("h264esp: session %q viewer %d left (total=%d)",
				s.name, id, len(subscribers))
			if len(subscribers) == 0 {
				s.hub.logger.Printf("h264esp: session %q last viewer left", s.name)
				return
			}

		case au, ok := <-s.encoder.AUs():
			if !ok {
				s.hub.logger.Printf("h264esp: session %q encoder ended", s.name)
				for _, sub := range subscribers {
					close(sub.frames)
				}
				return
			}
			for _, sub := range subscribers {
				select {
				case sub.frames <- au:
				default:
					sub.drops.Record(errors.New("viewer frames channel full"))
				}
			}
		}
	}
}

// teardown is called from run() on exit (any reason).
func (s *session) teardown() {
	s.cancel()
	_ = s.encoder.Close()
	s.upstream.Close()
	s.hub.removeSession(s.name, s)
	s.hub.logger.Printf("h264esp: session %q stopped", s.name)
}

// Subscriber represents one connected H.264 HTTP viewer. Obtain via
// [Hub.Subscribe]; release with [Subscriber.Close] when the HTTP
// response is over (client disconnect, error, server shutdown).
type Subscriber struct {
	id      uint64
	frames  chan []byte
	drops   *droplog.Counter
	session *session
	once    sync.Once
}

// ID returns the unique-per-session viewer id.
func (s *Subscriber) ID() uint64 { return s.id }

// Frames returns the read-only stream of Annex-B AUs for this viewer.
// One element = one complete AU = one HTTP response chunk (the
// briefing's wire shape).
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

// isClosed returns true if ch has been closed.
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
