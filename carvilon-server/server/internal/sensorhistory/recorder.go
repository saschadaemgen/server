package sensorhistory

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Recorder is the STORED path's aggregator, kept strictly independent of the
// LIVE path: it receives each reading via Record - a NON-BLOCKING hand-off,
// so the caller's real-time delivery to the editor block + cockpit is never
// delayed or throttled - accumulates readings into the current per-(device,
// metric) interval bucket, and flushes the MEAN to the Store at each interval
// boundary. A background sweep deletes samples past each device's retention.
//
// No reading in a bucket -> no sample written (an honest gap, not invented
// data). One reading -> the mean is that reading.
type Recorder struct {
	store *Store
	cfg   *ConfigStore
	now   func() time.Time
	log   *slog.Logger

	ch      chan reading
	dropped atomic.Int64

	mu      sync.Mutex
	buckets map[string]*bucket // device+"\x00"+metric -> accumulator

	pruneEvery time.Duration
}

type reading struct {
	deviceID string
	metric   string
	value    float64
	at       time.Time
}

// bucket accumulates the readings of one (device, metric) in the current
// interval window [start, start+interval).
type bucket struct {
	deviceID string
	metric   string
	start    int64 // bucket start, unix ms
	interval int64 // ms
	sum      float64
	n        int
}

// RecorderConfig configures a Recorder.
type RecorderConfig struct {
	Store      *Store
	Config     *ConfigStore
	Now        func() time.Time
	Log        *slog.Logger
	Buffer     int           // ingest channel capacity (default 4096)
	PruneEvery time.Duration // retention-sweep cadence (default 15 min)
}

// NewRecorder builds a Recorder. Call Run to start the aggregation loop.
func NewRecorder(rc RecorderConfig) *Recorder {
	if rc.Now == nil {
		rc.Now = time.Now
	}
	if rc.Log == nil {
		rc.Log = slog.Default()
	}
	if rc.Buffer <= 0 {
		rc.Buffer = 4096
	}
	if rc.PruneEvery <= 0 {
		rc.PruneEvery = 15 * time.Minute
	}
	return &Recorder{
		store:      rc.Store,
		cfg:        rc.Config,
		now:        rc.Now,
		log:        rc.Log,
		ch:         make(chan reading, rc.Buffer),
		buckets:    map[string]*bucket{},
		pruneEvery: rc.PruneEvery,
	}
}

// Record hands one reading to the recorder. It NEVER blocks: if the ingest
// buffer is full it drops the reading (recording is best-effort - the live
// path already delivered this value) and bumps a counter the loop logs. This
// is what keeps storage from ever throttling the real-time output.
func (r *Recorder) Record(deviceID, metric string, value float64, at time.Time) {
	select {
	case r.ch <- reading{deviceID: deviceID, metric: metric, value: value, at: at}:
	default:
		r.dropped.Add(1)
	}
}

// Run consumes readings, flushes elapsed buckets, and prunes retention until
// ctx is cancelled, then flushes any partial buckets so a clean shutdown does
// not lose the current interval.
func (r *Recorder) Run(ctx context.Context) {
	flush := time.NewTicker(time.Second) // fine intervals go down to 1 s
	defer flush.Stop()
	prune := time.NewTicker(r.pruneEvery)
	defer prune.Stop()
	// Prune once at startup so an old DB does not carry stale rows until the
	// first sweep tick.
	r.pruneRetention(ctx)
	for {
		select {
		case <-ctx.Done():
			r.flushAll(context.Background())
			return
		case rd := <-r.ch:
			r.ingest(ctx, rd)
		case <-flush.C:
			r.flushElapsed(ctx)
			if d := r.dropped.Swap(0); d > 0 {
				r.log.Warn("sensor history: readings dropped (ingest buffer full)", "count", d)
			}
		case <-prune.C:
			r.pruneRetention(ctx)
		}
	}
}

// ingest folds one reading into its bucket, flushing and rotating the bucket
// when the reading falls in a later interval window (a boundary crossing, or
// a changed interval config).
func (r *Recorder) ingest(ctx context.Context, rd reading) {
	cfg := r.cfg.Get(rd.deviceID)
	intervalMs := cfg.Interval.Milliseconds()
	if intervalMs < 1 {
		intervalMs = 1
	}
	start := (rd.at.UnixMilli() / intervalMs) * intervalMs
	key := rd.deviceID + "\x00" + rd.metric

	r.mu.Lock()
	b := r.buckets[key]
	if b != nil && b.start != start {
		old := *b
		delete(r.buckets, key)
		r.mu.Unlock()
		r.writeBucket(ctx, old)
		r.mu.Lock()
		b = nil
	}
	if b == nil {
		b = &bucket{deviceID: rd.deviceID, metric: rd.metric, start: start, interval: intervalMs}
		r.buckets[key] = b
	}
	b.sum += rd.value
	b.n++
	r.mu.Unlock()
}

// flushElapsed writes and removes every bucket whose interval window has
// ended, so a sensor that goes quiet still gets its last partial bucket
// persisted at the boundary (not held forever).
func (r *Recorder) flushElapsed(ctx context.Context) {
	nowMs := r.now().UnixMilli()
	var due []bucket
	r.mu.Lock()
	for key, b := range r.buckets {
		if nowMs >= b.start+b.interval {
			due = append(due, *b)
			delete(r.buckets, key)
		}
	}
	r.mu.Unlock()
	for _, b := range due {
		r.writeBucket(ctx, b)
	}
}

// flushAll writes every pending bucket (clean-shutdown final flush).
func (r *Recorder) flushAll(ctx context.Context) {
	var all []bucket
	r.mu.Lock()
	for key, b := range r.buckets {
		all = append(all, *b)
		delete(r.buckets, key)
	}
	r.mu.Unlock()
	for _, b := range all {
		r.writeBucket(ctx, b)
	}
}

// writeBucket persists one bucket's mean (skips an empty bucket - no reading,
// no sample).
func (r *Recorder) writeBucket(ctx context.Context, b bucket) {
	if b.n == 0 {
		return
	}
	sm := Sample{DeviceID: b.deviceID, Metric: b.metric, TS: b.start, Value: b.sum / float64(b.n), N: b.n}
	if err := r.store.Insert(ctx, sm); err != nil {
		r.log.Warn("sensor history: flush failed", "err", err)
	}
}

// pruneRetention deletes, per device, every sample older than that device's
// resolved retention (the §4 auto-delete). Rolling older data up to coarser
// buckets before deletion is the flagged follow-up the n column supports.
func (r *Recorder) pruneRetention(ctx context.Context) {
	devs, err := r.store.Devices(ctx)
	if err != nil {
		r.log.Warn("sensor history: retention list failed", "err", err)
		return
	}
	nowMs := r.now().UnixMilli()
	var total int64
	for _, d := range devs {
		cutoff := nowMs - r.cfg.Get(d).Retention.Milliseconds()
		n, err := r.store.Prune(ctx, d, cutoff)
		if err != nil {
			r.log.Warn("sensor history: retention prune failed", "err", err)
			continue
		}
		total += n
	}
	if total > 0 {
		r.log.Info("sensor history: retention pruned old samples", "rows", total)
	}
}
