// Package streamstore holds the edge-side store for the cloud's live
// WHEP-subscriber ("cloud viewer") counts - the egress mirror of turnstore's
// snapshot side. The cloud (VPS) counts consumers per stream and pushes a
// periodic snapshot over the side-channel; the edge caches the latest one
// here, stamped with its OWN receive time, for the admin dashboard.
//
// It is snapshot-only (no SQLite history, unlike turnstore): a consumer count
// is a live gauge, not an event log. SnapshotHolder + Freshness mirror
// turnstore's exactly, so the dashboard renders the same honest "Stand vor Xs"
// and can tell "cloud reachable, 0 viewers" (a fresh snapshot, no streams)
// from "cloud unreachable" (a stale snapshot).
package streamstore

import (
	"sync"
	"time"
)

// DefaultStaleAfter mirrors turnstore.DefaultStaleAfter: 3x the cloud's 10s
// snapshot tick, so a single missed push does not flip the UI to "stale" but
// a real cloud outage does.
const DefaultStaleAfter = 30 * time.Second

// Stat is one stream's live WHEP-subscriber (consumer) count. StreamID is the
// viewer MAC the cloud WHIP/WHEP routes by (the same identity the side-channel
// Envelope.StreamID carries), so the edge resolves it to a viewer/profile
// without any extra mapping.
type Stat struct {
	StreamID  string `json:"stream_id"`
	Consumers int    `json:"consumers"`
}

// Snapshot is a point-in-time set of per-stream consumer counts plus the VPS
// send time. Only streams with at least one consumer appear; an empty Streams
// on a fresh snapshot means "cloud reachable, zero viewers".
type Snapshot struct {
	GeneratedAt time.Time `json:"generated_at"`
	Streams     []Stat    `json:"streams,omitempty"`
}

// SnapshotHolder holds the most recent snapshot the cloud pushed, plus the
// edge receive time it arrived at. Goroutine-safe: the side-channel read loop
// writes it, the admin handler reads it. Mirrors turnstore.SnapshotHolder.
type SnapshotHolder struct {
	mu         sync.Mutex
	snap       Snapshot
	receivedAt time.Time
	present    bool
}

// NewSnapshotHolder returns an empty holder (no snapshot yet).
func NewSnapshotHolder() *SnapshotHolder { return &SnapshotHolder{} }

// Set stores a snapshot and the edge-local time it was received. The receive
// time (not the snapshot's VPS GeneratedAt) is the freshness clock, so
// cross-machine clock skew cannot make a fresh link look stale.
func (h *SnapshotHolder) Set(snap Snapshot, receivedAt time.Time) {
	h.mu.Lock()
	h.snap = snap
	h.receivedAt = receivedAt
	h.present = true
	h.mu.Unlock()
}

// Get returns the last snapshot, its edge receive time, and whether any
// snapshot has been received yet.
func (h *SnapshotHolder) Get() (snap Snapshot, receivedAt time.Time, present bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snap, h.receivedAt, h.present
}

// Freshness reports a snapshot's age in seconds and whether it is stale, using
// one clock only (the edge's): age = now - receivedAt. Mirrors
// turnstore.Freshness so the dashboard shows the same "Stand vor Xs".
func Freshness(receivedAt, now time.Time, staleAfter time.Duration) (ageSeconds int, stale bool) {
	age := now.Sub(receivedAt)
	if age < 0 {
		age = 0
	}
	return int(age.Seconds()), age > staleAfter
}
