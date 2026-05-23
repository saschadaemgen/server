// Package sourcereg manages a set of [hub.Hub]s keyed by camera
// identity, so that the same camera+quality pull is shared by all
// consumers and idle cameras cost nothing.
//
// Design (S4-01):
//
//   - Each (CameraID, Quality) maps to its own [hub.Hub].
//   - Hubs are built lazily on first [Registry.HubFor] call; they are
//     kept in the registry map for the registry's lifetime.
//   - The hub itself owns the source lifecycle: it builds + starts the
//     [source.VideoSource] only when it has at least one subscriber,
//     and closes it as soon as the subscriber count drops to zero.
//     A subsequent subscribe rebuilds via the factory.
//   - When NO subscribers exist for a key, there is no RTSP pull, no
//     decode, and no ffmpeg — the hub is an idle goroutine waiting on
//     channels. That is the "0 Last bei 0 Zuschauern"-Anforderung from
//     the briefing: the only resident cost per registered key is the
//     hub bookkeeping (a small struct + one parked goroutine).
//
// Multiple consumers (the WebRTC handleOffer goroutines and the
// MJPEG encoder forwarder for that same camera) attach to the SAME
// hub returned by HubFor and so share a single upstream RTSP pull.
package sourcereg

import (
	"fmt"
	"log"
	"sync"

	"carvilon.local/stream/internal/hub"
	"carvilon.local/stream/internal/source"
)

// Key identifies a unique upstream pull. Sized so that distinct
// transport configurations get distinct hubs:
//
//   - CameraID + Quality — same camera, different quality tiers go
//     to different pulls (UniFi delivers distinct streams per tier).
//   - Encryption — S6-12. Two profiles on the same (CameraID,Quality)
//     but with different encryption modes get DIFFERENT hubs.
//     Without this split, the first-arriving subscriber's mode would
//     silently win for everyone joining the same hub later, and a
//     well-meaning "tune one profile to srtp" would either get plain
//     RTP that fails to decrypt or vice versa. The cost is one extra
//     ffmpeg/source pull when an admin runs a mixed-mode setup — a
//     rare and intentional case.
//
// Callers should pass the canonical Encryption value ("tls" or
// "srtp", not ""); the source factory normalises "" → "tls" at the
// streaming-server boundary before building the Key.
type Key struct {
	CameraID   string
	Quality    string
	Encryption string
}

// String renders the key for log lines.
func (k Key) String() string {
	return fmt.Sprintf("%s:%s/%s", k.CameraID, k.Quality, k.Encryption)
}

// Factory builds a fresh, un-Started [source.VideoSource] for the
// given key. Called by the hub at the moment a subscriber arrives on
// an idle hub.
type Factory func(Key) (source.VideoSource, error)

// Registry holds a lazy map of camera-keyed hubs.
type Registry struct {
	factory Factory
	logger  *log.Logger

	mu   sync.Mutex
	hubs map[Key]*hub.Hub
}

// New returns a Registry. The factory is invoked lazily — pre-existing
// hubs are never created at startup, only on first HubFor call per key.
func New(factory Factory, logger *log.Logger) *Registry {
	if factory == nil {
		panic("sourcereg: factory must not be nil")
	}
	if logger == nil {
		logger = log.Default()
	}
	return &Registry{
		factory: factory,
		logger:  logger,
		hubs:    make(map[Key]*hub.Hub),
	}
}

// HubFor returns the hub for the given key, creating it on first call.
// Subsequent calls with the same key return the same hub — that is how
// independent consumers (WebRTC, MJPEG, future MJPEG profiles) share
// one upstream pull.
//
// Note: the returned hub is created idle. The actual source pull only
// starts when a subscriber arrives via [hub.Hub.Subscribe].
func (r *Registry) HubFor(key Key) *hub.Hub {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.hubs[key]; ok {
		return h
	}
	keyLocal := key // capture by value for the closure
	h := hub.New(func() (source.VideoSource, error) {
		return r.factory(keyLocal)
	}, hub.Options{Logger: r.logger})
	r.hubs[key] = h
	r.logger.Printf("sourcereg: hub created for %s (idle until first subscriber)", key)
	return h
}

// Has reports whether the registry has a hub registered for the key.
// Useful for tests and instrumentation.
func (r *Registry) Has(key Key) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.hubs[key]
	return ok
}

// Keys returns the registered keys. Order is not guaranteed.
func (r *Registry) Keys() []Key {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Key, 0, len(r.hubs))
	for k := range r.hubs {
		out = append(out, k)
	}
	return out
}

// Close shuts every registered hub down. After Close the registry is
// not reusable (it will not build new hubs — HubFor would, but Close
// is meant to be called only at program shutdown).
func (r *Registry) Close() error {
	r.mu.Lock()
	hubs := make([]*hub.Hub, 0, len(r.hubs))
	for _, h := range r.hubs {
		hubs = append(hubs, h)
	}
	r.hubs = nil
	r.mu.Unlock()

	for _, h := range hubs {
		_ = h.Close()
	}
	return nil
}
