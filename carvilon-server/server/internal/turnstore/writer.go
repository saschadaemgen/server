package turnstore

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Default tuning. Retention is the 30-day window (Sascha decision);
// DefaultStaleAfter is the live-stats freshness threshold, set to 3x
// the cloud's 10s snapshot tick so a single missed push does not flip
// the UI to "stale" but a real cloud outage does.
const (
	DefaultRetention     = 30 * 24 * time.Hour
	DefaultStaleAfter    = 30 * time.Second
	defaultBufferSize    = 256
	defaultPurgeInterval = time.Hour
)

// Options tunes a Writer. Zero values fall back to the Default*
// constants; production passes the zero Options, tests size the
// buffer up so a burst of concurrent submits cannot be dropped.
type Options struct {
	Retention     time.Duration
	BufferSize    int
	PurgeInterval time.Duration
}

// Writer serialises all SQLite writes through a single goroutine. The
// callbacks that feed it (the side-channel read loop for TURN events,
// the whipclient's ICE-state callback) may run concurrently from pion
// goroutines, so Submit* are non-blocking channel sends: a full buffer
// drops the event with a warn rather than stalling the relay or the
// read loop. Run owns the only DB-writing goroutine.
type Writer struct {
	store     *Store
	log       *slog.Logger
	events    chan Event
	ice       chan ICEEvent
	retention time.Duration
	purgeIvl  time.Duration
}

// NewWriter builds a Writer. Call Run in its own goroutine.
func NewWriter(store *Store, log *slog.Logger, opts Options) *Writer {
	if opts.Retention <= 0 {
		opts.Retention = DefaultRetention
	}
	if opts.BufferSize <= 0 {
		opts.BufferSize = defaultBufferSize
	}
	if opts.PurgeInterval <= 0 {
		opts.PurgeInterval = defaultPurgeInterval
	}
	return &Writer{
		store:     store,
		log:       log,
		events:    make(chan Event, opts.BufferSize),
		ice:       make(chan ICEEvent, opts.BufferSize),
		retention: opts.Retention,
		purgeIvl:  opts.PurgeInterval,
	}
}

// SubmitEvent enqueues a TURN event for persistence. Non-blocking:
// drops with a warn when the buffer is full.
func (w *Writer) SubmitEvent(e Event) {
	select {
	case w.events <- e:
	default:
		w.log.Warn("turnstore writer buffer full, dropping turn event", "kind", e.Kind)
	}
}

// SubmitICE enqueues an ICE-state event for persistence. Non-blocking.
func (w *Writer) SubmitICE(e ICEEvent) {
	select {
	case w.ice <- e:
	default:
		w.log.Warn("turnstore writer buffer full, dropping ice event", "stream_id", e.StreamID)
	}
}

// Run drains the submit channels into SQLite from a single goroutine
// and runs the retention purge on an interval (plus once at start). It
// blocks until ctx is cancelled. A failed insert only logs; the writer
// keeps draining (one bad row must not stall the queue).
func (w *Writer) Run(ctx context.Context) {
	ticker := time.NewTicker(w.purgeIvl)
	defer ticker.Stop()
	w.purgeNow(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-w.events:
			if err := w.store.InsertEvent(ctx, e); err != nil {
				w.log.Warn("turnstore insert event failed", "err", err)
			}
		case e := <-w.ice:
			if err := w.store.InsertICEEvent(ctx, e); err != nil {
				w.log.Warn("turnstore insert ice event failed", "err", err)
			}
		case <-ticker.C:
			w.purgeNow(ctx)
		}
	}
}

func (w *Writer) purgeNow(ctx context.Context) {
	n, err := w.store.Purge(ctx, time.Now().Add(-w.retention))
	if err != nil {
		w.log.Warn("turnstore purge failed", "err", err)
		return
	}
	if n > 0 {
		w.log.Info("turnstore retention purge", "rows", n, "retention", w.retention.String())
	}
}

// SnapshotHolder holds the most recent live snapshot the cloud pushed,
// plus the edge receive time it arrived at. Goroutine-safe: the
// side-channel read loop writes it, the admin handler reads it.
type SnapshotHolder struct {
	mu         sync.Mutex
	snap       Snapshot
	receivedAt time.Time
	present    bool
}

// NewSnapshotHolder returns an empty holder (no snapshot yet).
func NewSnapshotHolder() *SnapshotHolder { return &SnapshotHolder{} }

// Set stores a snapshot and the edge-local time it was received. The
// receive time (not the snapshot's VPS GeneratedAt) is the freshness
// clock, so cross-machine clock skew cannot make a fresh link look
// stale.
func (h *SnapshotHolder) Set(snap Snapshot, receivedAt time.Time) {
	h.mu.Lock()
	h.snap = snap
	h.receivedAt = receivedAt
	h.present = true
	h.mu.Unlock()
}

// Get returns the last snapshot, its edge receive time, and whether
// any snapshot has been received yet.
func (h *SnapshotHolder) Get() (snap Snapshot, receivedAt time.Time, present bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snap, h.receivedAt, h.present
}

// Freshness reports a snapshot's age in seconds and whether it is
// stale. It uses one clock only (the edge's): age = now - receivedAt.
// A snapshot older than staleAfter is stale, so the UI shows
// "veraltet" / "Cloud nicht erreichbar" instead of presenting an old
// number as current.
func Freshness(receivedAt, now time.Time, staleAfter time.Duration) (ageSeconds int, stale bool) {
	age := now.Sub(receivedAt)
	if age < 0 {
		age = 0
	}
	return int(age.Seconds()), age > staleAfter
}
