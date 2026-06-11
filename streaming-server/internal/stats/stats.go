// Package stats accumulates per-client and global throughput metrics
// for the streaming-server.
//
// Purpose (S6-01): the ESP measurement campaign needs apples-to-apples
// numbers across MJPEG variants and the new H.264-CBP transcode on the
// same camera. Two questions drive every measurement:
//
//   - How many bytes per frame does the wire actually carry?
//     (frames_sent + bytes_sent on a real client, observed at the HTTP
//     write boundary — NOT estimated from encoder configuration.)
//   - How expensive is the transcoder for the chosen settings?
//     (handled by internal/proccpu; surfaced here as global.cpu_percent
//     for symmetry.)
//
// The registry is intentionally append-only on the hot path: every
// per-frame call is an [atomic.Int64.Add] on the Client's own counters,
// no lock. The map lock is only taken on Register / Unregister /
// Snapshot — the connect/disconnect rate is low enough that an
// RWMutex is the right tool.
//
// Cumulative-since-connect averages: avg_fps and avg_bitrate_kbps are
// computed at Snapshot time from total_frames / uptime and
// total_bytes*8 / uptime. We deliberately do NOT keep a sliding window
// in this first cut — the measurement workflow is "PUT new tuning,
// reconnect, watch fresh numbers", so cumulative numbers map cleanly
// onto a measurement run. A rolling window can be added later without
// breaking the JSON shape (the field semantics would just sharpen).
package stats

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Client is one tracked viewer — typically one HTTP connection
// (handleMJPEG / handleH264 / ...). Obtain via [Registry.Register];
// release with [Registry.Unregister] (typically `defer`) once the HTTP
// response is over.
//
// All counter fields are atomic; concurrent RecordFrame / RecordDrop
// calls from any goroutine are safe.
type Client struct {
	// Immutable identification — populated by Register, never written
	// again.
	ID          uint64
	Profile     string
	Codec       string
	RemoteAddr  string
	ConnectedAt time.Time
	// Uplink marks an in-process publish bridge (the WHIP cloud push,
	// S20) instead of a real viewer. Uplink senders carry full
	// throughput counters — they ARE edge egress — but are counted
	// separately (ProfileSnapshot.Uplinks / GlobalSnapshot.Uplinks),
	// never in the Clients consumer counts, so a publish session can
	// not masquerade as a LAN viewer.
	Uplink bool

	// Hot-path counters.
	framesSent    atomic.Int64
	framesDropped atomic.Int64
	bytesSent     atomic.Int64
	lastFrameNs   atomic.Int64 // unix-nano of most recent RecordFrame
}

// RecordFrame is the per-frame instrumentation call. Increment by one
// frame, add the byte count, and remember the wall-clock so the
// snapshot can show staleness if the client is wedged.
//
// Safe to call from multiple goroutines (only one HTTP handler writes
// per Client today, but the lock-free counters make this future-proof).
func (c *Client) RecordFrame(bytes int) {
	if c == nil {
		return
	}
	c.framesSent.Add(1)
	c.bytesSent.Add(int64(bytes))
	c.lastFrameNs.Store(time.Now().UnixNano())
}

// RecordDrop counts a frame that was prepared but could not be
// delivered to this client (e.g. HTTP write error, full subscriber
// buffer at the hub layer). The current handleMJPEG calls this when
// the HTTP write returns an error and the connection is torn down;
// the per-viewer hub drops are still logged separately via droplog.
func (c *Client) RecordDrop() {
	if c == nil {
		return
	}
	c.framesDropped.Add(1)
}

// ClientSnapshot is the JSON shape returned by /stream/stats per
// connected client. Times are RFC3339 strings so they survive JSON
// round-trips without timezone games; uptime / averages are scalar
// numbers so an admin UI doesn't have to do its own arithmetic.
type ClientSnapshot struct {
	ID             uint64  `json:"id"`
	Profile        string  `json:"profile"`
	Codec          string  `json:"codec"`
	RemoteAddr     string  `json:"remote_addr"`
	ConnectedAt    string  `json:"connected_at"`
	UptimeSec      float64 `json:"uptime_sec"`
	FramesSent     int64   `json:"frames_sent"`
	FramesDropped  int64   `json:"frames_dropped"`
	BytesSent      int64   `json:"bytes_sent"`
	AvgFPS         float64 `json:"avg_fps"`
	AvgBitrateKbps float64 `json:"avg_bitrate_kbps"`
	LastFrameAt    string  `json:"last_frame_at,omitempty"`
	Uplink         bool    `json:"uplink,omitempty"` // publish bridge, not a viewer (S20)
}

// snapshot captures the Client's current counter values into the
// JSON-friendly snapshot. Called under Registry.mu (read lock).
func (c *Client) snapshot(now time.Time) ClientSnapshot {
	frames := c.framesSent.Load()
	bytes := c.bytesSent.Load()
	uptime := now.Sub(c.ConnectedAt).Seconds()
	if uptime <= 0 {
		uptime = 0.000001 // avoid div-by-zero on the first sample
	}
	var lastFrame string
	if ns := c.lastFrameNs.Load(); ns > 0 {
		lastFrame = time.Unix(0, ns).UTC().Format(time.RFC3339Nano)
	}
	return ClientSnapshot{
		ID:             c.ID,
		Profile:        c.Profile,
		Codec:          c.Codec,
		RemoteAddr:     c.RemoteAddr,
		ConnectedAt:    c.ConnectedAt.UTC().Format(time.RFC3339Nano),
		UptimeSec:      uptime,
		FramesSent:     frames,
		FramesDropped:  c.framesDropped.Load(),
		BytesSent:      bytes,
		AvgFPS:         float64(frames) / uptime,
		AvgBitrateKbps: float64(bytes) * 8 / 1000 / uptime,
		LastFrameAt:    lastFrame,
		Uplink:         c.Uplink,
	}
}

// ProfileSnapshot aggregates all clients of one profile name. The
// admin UI uses this for the per-profile rows; the JSON tags match the
// per-client shape where the semantic is identical so a renderer can
// share code.
//
// SourceFrames / SourceFPS are S6-04: how many upstream Access Units
// reached the encoder for this profile, and at what rate. The
// /stream/stats consumer compares them against the per-client AvgFPS
// to localise where frames are lost — Kamera vs Encoder vs Wire.
// Both fields are populated only while a session is active; cleared
// at session start (= first subscriber arrives), so each measurement
// run sees a clean window.
type ProfileSnapshot struct {
	Profile string `json:"profile"`
	Codec   string `json:"codec"`
	// Clients counts real viewers only. Uplinks counts in-process
	// publish bridges (WHIP cloud push, S20). The throughput fields
	// below aggregate over BOTH — they are the profile's total egress —
	// so a publish-only profile still shows live output/bitrate/drops
	// while its consumer count honestly stays 0.
	Clients        int     `json:"clients"`
	Uplinks        int     `json:"uplinks,omitempty"`
	FramesSent     int64   `json:"frames_sent"`
	FramesDropped  int64   `json:"frames_dropped"`
	BytesSent      int64   `json:"bytes_sent"`
	AvgFPS         float64 `json:"avg_fps"`
	AvgBitrateKbps float64 `json:"avg_bitrate_kbps"`
	SourceFrames   int64   `json:"source_frames,omitempty"`
	SourceFPS      float64 `json:"source_fps,omitempty"`
}

// GlobalSnapshot is the server-wide aggregate. TranscoderCPUPercent
// is filled in by the server from internal/proccpu — stats itself
// doesn't sample CPU. Clients counts real viewers only; Uplinks counts
// publish bridges. Frames/bytes totals cover both (all edge egress).
type GlobalSnapshot struct {
	Clients              int     `json:"clients"`
	Uplinks              int     `json:"uplinks,omitempty"`
	FramesSentTotal      int64   `json:"frames_sent_total"`
	BytesSentTotal       int64   `json:"bytes_sent_total"`
	TranscoderCPUPercent float64 `json:"transcoder_cpu_percent,omitempty"`
}

// Snapshot is the full /stream/stats document. The server marshals
// this directly via encoding/json.
type Snapshot struct {
	GeneratedAt string                     `json:"generated_at"`
	Global      GlobalSnapshot             `json:"global"`
	Profiles    map[string]ProfileSnapshot `json:"profiles"`
	Clients     []ClientSnapshot           `json:"clients"`
}

// Registry is the central client tracker. Pass one into the server;
// every HTTP handler that streams frames registers / unregisters / and
// calls RecordFrame on the returned Client.
//
// The zero value is NOT ready — use [New].
type Registry struct {
	mu      sync.RWMutex
	clients map[uint64]*Client
	nextID  uint64

	// S6-04: per-profile source counters, tracked independently of
	// the per-client output counters. The hubs (mjpeg / h264esp) call
	// RecordSourceFrame as upstream AUs arrive and ResetSourceCounter
	// when a fresh encoder session starts.
	profMu          sync.Mutex
	profileCounters map[string]*profileCounter
}

// profileCounter is the per-profile source-side state. Pointer
// receivers + atomic fields so the hot RecordSourceFrame path stays
// lock-free once the counter is created.
type profileCounter struct {
	sourceFrames   atomic.Int64
	sessionStartNs atomic.Int64
}

// New returns a ready-to-use Registry.
func New() *Registry {
	return &Registry{
		clients:         make(map[uint64]*Client),
		profileCounters: make(map[string]*profileCounter),
	}
}

// Register creates a Client and adds it to the registry. The returned
// pointer is the handle the caller uses for RecordFrame / RecordDrop;
// call [Registry.Unregister] once the HTTP response is over.
//
// Allocating one *Client per HTTP connection is intentional: it keeps
// the hot-path counters on a cache line owned by the connection's
// goroutine.
func (r *Registry) Register(profileName, codec, remoteAddr string) *Client {
	return r.register(profileName, codec, remoteAddr, false)
}

// RegisterUplink creates a Client marked as an in-process publish bridge
// (the WHIP cloud push, S20). It carries the same throughput counters as
// a viewer — the uplink IS edge egress — but Snapshot counts it under
// Uplinks, never under the Clients consumer counts. label takes the
// RemoteAddr slot (there is no peer address for an in-process bridge).
func (r *Registry) RegisterUplink(profileName, codec, label string) *Client {
	return r.register(profileName, codec, label, true)
}

func (r *Registry) register(profileName, codec, remoteAddr string, uplink bool) *Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	c := &Client{
		ID:          r.nextID,
		Profile:     profileName,
		Codec:       codec,
		RemoteAddr:  remoteAddr,
		ConnectedAt: time.Now(),
		Uplink:      uplink,
	}
	r.clients[c.ID] = c
	return c
}

// Unregister removes a client from the registry. Idempotent — calling
// twice (or with a Client from a different Registry) is a no-op so
// `defer reg.Unregister(c)` is always safe.
func (r *Registry) Unregister(c *Client) {
	if c == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.clients, c.ID)
}

// Snapshot returns the JSON-ready view of the current state. Safe to
// call concurrently with Register / Unregister / RecordFrame.
//
// The Clients slice is sorted by ID (= connection order) so the
// admin UI gets a stable ordering across snapshots. The Profiles map
// uses the profile name as key.
func (r *Registry) Snapshot() Snapshot {
	now := time.Now()
	r.mu.RLock()
	defer r.mu.RUnlock()

	clientSnaps := make([]ClientSnapshot, 0, len(r.clients))
	for _, c := range r.clients {
		clientSnaps = append(clientSnaps, c.snapshot(now))
	}
	sort.Slice(clientSnaps, func(i, j int) bool {
		return clientSnaps[i].ID < clientSnaps[j].ID
	})

	// Aggregate per profile + globally. Viewers and uplinks are counted
	// apart (consumer counts must never include the publish bridge), but
	// the throughput sums cover both — they are the profile's egress.
	profiles := make(map[string]ProfileSnapshot)
	var globalFrames, globalBytes int64
	var globalClients, globalUplinks int
	for _, cs := range clientSnaps {
		ps := profiles[cs.Profile]
		ps.Profile = cs.Profile
		ps.Codec = cs.Codec
		if cs.Uplink {
			ps.Uplinks++
			globalUplinks++
		} else {
			ps.Clients++
			globalClients++
		}
		ps.FramesSent += cs.FramesSent
		ps.FramesDropped += cs.FramesDropped
		ps.BytesSent += cs.BytesSent
		profiles[cs.Profile] = ps

		globalFrames += cs.FramesSent
		globalBytes += cs.BytesSent
	}
	// Profile-level averages: weighted by per-client uptime would be
	// most accurate, but the per-client uptimes are already in the
	// ClientSnapshot. Here we use a simple aggregate: total frames /
	// total client-seconds, total bytes*8 / total client-seconds. This
	// converges to "average per client" if all clients have similar
	// uptime, which is the common case during a measurement run.
	clientSeconds := make(map[string]float64)
	for _, cs := range clientSnaps {
		clientSeconds[cs.Profile] += cs.UptimeSec
	}
	for name, ps := range profiles {
		secs := clientSeconds[name]
		if secs > 0 {
			ps.AvgFPS = float64(ps.FramesSent) / secs
			ps.AvgBitrateKbps = float64(ps.BytesSent) * 8 / 1000 / secs
		}
		profiles[name] = ps
	}

	// S6-04: enrich existing per-profile snapshots with source-side
	// counters. We only attach to profiles that already have senders
	// (viewers or uplinks, and therefore an active session) — a counter
	// that outlived its session would otherwise show stale data.
	r.profMu.Lock()
	for name, pc := range r.profileCounters {
		ps, ok := profiles[name]
		if !ok {
			continue
		}
		ps.SourceFrames = pc.sourceFrames.Load()
		if startNs := pc.sessionStartNs.Load(); startNs > 0 {
			elapsedSec := float64(now.UnixNano()-startNs) / 1e9
			if elapsedSec > 0 {
				ps.SourceFPS = float64(ps.SourceFrames) / elapsedSec
			}
		}
		profiles[name] = ps
	}
	r.profMu.Unlock()

	return Snapshot{
		GeneratedAt: now.UTC().Format(time.RFC3339Nano),
		Global: GlobalSnapshot{
			Clients:         globalClients,
			Uplinks:         globalUplinks,
			FramesSentTotal: globalFrames,
			BytesSentTotal:  globalBytes,
		},
		Profiles: profiles,
		Clients:  clientSnaps,
	}
}

// Count returns the number of currently registered clients. Cheap;
// uses the registry's read lock only.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.clients)
}

// RecordSourceFrame counts one upstream Access Unit for the given
// profile — call once per AU the per-codec hub's session forwarder
// receives from the shared camera bus. Cheap (one atomic increment
// once the counter exists). Nil-safe.
//
// The counter is created lazily on first call. The session-start
// timestamp is also set on first call; subsequent calls leave it
// alone so the rate calculation in Snapshot uses a stable window.
func (r *Registry) RecordSourceFrame(profile string) {
	if r == nil || profile == "" {
		return
	}
	pc := r.profileCounter(profile)
	if pc.sessionStartNs.Load() == 0 {
		pc.sessionStartNs.CompareAndSwap(0, time.Now().UnixNano())
	}
	pc.sourceFrames.Add(1)
}

// ResetSourceCounter clears the source counter for the given profile.
// Called by the per-codec hubs at session start (= first subscriber
// arrives, encoder is about to launch) so each measurement run sees
// a fresh rate window — otherwise a long-idle counter would drag the
// avg_source_fps down and confuse the operator.
//
// Nil-safe; calling with an unknown profile is a no-op.
func (r *Registry) ResetSourceCounter(profile string) {
	if r == nil || profile == "" {
		return
	}
	pc := r.profileCounter(profile)
	pc.sourceFrames.Store(0)
	pc.sessionStartNs.Store(0)
}

// profileCounter looks up the per-profile counter, creating it on
// first use. Holds profMu only for the (typically once-per-profile)
// map insert.
func (r *Registry) profileCounter(profile string) *profileCounter {
	r.profMu.Lock()
	defer r.profMu.Unlock()
	pc, ok := r.profileCounters[profile]
	if !ok {
		pc = &profileCounter{}
		r.profileCounters[profile] = pc
	}
	return pc
}
