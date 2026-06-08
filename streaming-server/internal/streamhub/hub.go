// Package streamhub manages active WHIP publishers and their RTP tracks
// for cloud-side fan-out.
//
// One streamID = at most one active publisher at any time. A second
// publish attempt for the same streamID is rejected with [ErrConflict];
// the upstream caller should tear the first one down (DELETE, later
// season) before re-publishing.
//
// The hub is the seam between WHIP ingress (S2-04, fills the hub) and
// WHEP egress (S2-05, reads the stored [webrtc.TrackLocalStaticRTP]).
package streamhub

import (
	"context"
	"errors"
	"sync"

	"github.com/pion/webrtc/v4"
)

// ErrConflict is returned by [Hub.Add] when the streamID already has an
// active publisher.
var ErrConflict = errors.New("streamhub: streamID already has an active publisher")

// Session represents one active WHIP publisher.
//
// Construct with [NewSession] — the zero value's ready channel is nil,
// which would make [Session.WaitTrack] block forever.
type Session struct {
	StreamID string
	PC       *webrtc.PeerConnection
	// Track is the fan-out source WHEP subscribers read from. It is set
	// exactly once, via [Session.SetTrack], when the publisher's first
	// RTP track arrives (OnTrack). Do NOT write it directly — readers
	// rely on the ready-channel happens-before to observe it safely.
	// Read it through [Session.WaitTrack], not the field, unless you
	// already know ready is closed.
	Track *webrtc.TrackLocalStaticRTP
	// OnClose runs exactly once, on the first [Hub.Remove] for this
	// session. Used to tear down the PeerConnection.
	OnClose func()

	// ready is closed by SetTrack once Track is set. WHEP subscribers
	// block on it (via WaitTrack) so they never attach a nil track in
	// the window between Add and the first OnTrack callback (S2-05
	// race fix).
	ready chan struct{}
	// trackOnce guarantees Track-set + ready-close happen exactly once,
	// even if OnTrack fires for multiple m-lines.
	trackOnce sync.Once
}

// NewSession creates a Session with an initialised ready channel. PC and
// onClose may be supplied now; Track arrives later via [Session.SetTrack].
func NewSession(streamID string, pc *webrtc.PeerConnection, onClose func()) *Session {
	return &Session{
		StreamID: streamID,
		PC:       pc,
		OnClose:  onClose,
		ready:    make(chan struct{}),
	}
}

// SetTrack stores the fan-out track and signals readiness. Effective
// exactly once; later calls are no-ops (the first track wins). Called
// from the WHIP OnTrack callback.
func (s *Session) SetTrack(t *webrtc.TrackLocalStaticRTP) {
	s.trackOnce.Do(func() {
		s.Track = t
		close(s.ready)
	})
}

// WaitTrack blocks until the track is ready or ctx is done. It returns
// the fan-out track, or ctx.Err() if the deadline/cancellation fires
// first. The channel-close happens-before guarantees the returned Track
// pointer is fully published to the caller.
func (s *Session) WaitTrack(ctx context.Context) (*webrtc.TrackLocalStaticRTP, error) {
	select {
	case <-s.ready:
		return s.Track, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Hub is the thread-safe registry of active publishers, keyed by
// streamID. The zero value is not usable — construct with [NewHub].
type Hub struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

// NewHub returns an empty, ready-to-use Hub.
func NewHub() *Hub {
	return &Hub{sessions: make(map[string]*Session)}
}

// Add registers a session. Returns [ErrConflict] if the streamID is
// already published (lazy single-publisher invariant).
func (h *Hub) Add(s *Session) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.sessions[s.StreamID]; exists {
		return ErrConflict
	}
	h.sessions[s.StreamID] = s
	return nil
}

// Get returns the active session for streamID, or (nil, false) if none.
func (h *Hub) Get(streamID string) (*Session, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	s, ok := h.sessions[streamID]
	return s, ok
}

// Remove deletes the session for streamID and invokes its OnClose
// exactly once. Idempotent: a second Remove (or a Remove for an unknown
// streamID) is a no-op and never double-fires OnClose.
//
// OnClose runs OUTSIDE the lock so a teardown that happens to re-enter
// the hub cannot deadlock.
func (h *Hub) Remove(streamID string) {
	h.mu.Lock()
	s, ok := h.sessions[streamID]
	if ok {
		delete(h.sessions, streamID)
	}
	h.mu.Unlock()

	if ok && s.OnClose != nil {
		s.OnClose()
	}
}
