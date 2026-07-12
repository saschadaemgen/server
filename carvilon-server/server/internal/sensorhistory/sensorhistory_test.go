package sensorhistory

import (
	"context"
	"math"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"carvilon.local/server/internal/db"
)

func newTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestStore_InsertQueryRaw(t *testing.T) {
	ctx := context.Background()
	st := New(newTestDB(t).DB)
	if err := st.Insert(ctx,
		Sample{DeviceID: "sen-1", Metric: "temperature", TS: 1000, Value: 21.0, N: 3},
		Sample{DeviceID: "sen-1", Metric: "temperature", TS: 2000, Value: 22.0, N: 4},
		Sample{DeviceID: "sen-1", Metric: "humidity", TS: 1000, Value: 50.0, N: 2},
	); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := st.Query(ctx, "sen-1", "temperature", 0, 5000, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 2 || got[0].TS != 1000 || got[1].Value != 22.0 || got[0].N != 3 {
		t.Fatalf("raw query wrong: %+v", got)
	}
	// metric + range filtering
	if h, _ := st.Query(ctx, "sen-1", "humidity", 0, 5000, 0); len(h) != 1 {
		t.Errorf("humidity should have 1 sample, got %d", len(h))
	}
	if r, _ := st.Query(ctx, "sen-1", "temperature", 1500, 5000, 0); len(r) != 1 {
		t.Errorf("range 1500.. should have 1 sample, got %d", len(r))
	}
}

func TestStore_InsertReplacesBucket(t *testing.T) {
	ctx := context.Background()
	st := New(newTestDB(t).DB)
	_ = st.Insert(ctx, Sample{DeviceID: "s", Metric: "m", TS: 1000, Value: 10, N: 1})
	_ = st.Insert(ctx, Sample{DeviceID: "s", Metric: "m", TS: 1000, Value: 20, N: 2})
	got, _ := st.Query(ctx, "s", "m", 0, 2000, 0)
	if len(got) != 1 || got[0].Value != 20 || got[0].N != 2 {
		t.Fatalf("re-insert should replace the bucket: %+v", got)
	}
}

func TestStore_QueryDownsampleWeightedMean(t *testing.T) {
	ctx := context.Background()
	st := New(newTestDB(t).DB)
	// 100 raw buckets at ts=i*1000, value=i, weight=1.
	samples := make([]Sample, 100)
	for i := range samples {
		samples[i] = Sample{DeviceID: "s", Metric: "m", TS: int64(i) * 1000, Value: float64(i), N: 1}
	}
	if err := st.Insert(ctx, samples...); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Raw query returns all 100.
	if raw, _ := st.Query(ctx, "s", "m", 0, 99000, 0); len(raw) != 100 {
		t.Fatalf("raw = %d, want 100", len(raw))
	}
	// Downsample to ~10 points.
	ds, err := st.Query(ctx, "s", "m", 0, 99000, 10)
	if err != nil {
		t.Fatalf("downsample query: %v", err)
	}
	if len(ds) == 100 || len(ds) > 12 {
		t.Fatalf("downsample len = %d, want a small bounded set", len(ds))
	}
	// Weighted totals must be conserved: sum of n over all downsampled
	// buckets == 100, and the overall weighted mean == mean(0..99)=49.5.
	var totN int
	var wsum float64
	for _, s := range ds {
		totN += s.N
		wsum += s.Value * float64(s.N)
	}
	if totN != 100 {
		t.Errorf("downsample total n = %d, want 100", totN)
	}
	if mean := wsum / float64(totN); math.Abs(mean-49.5) > 1e-9 {
		t.Errorf("downsample weighted mean = %v, want 49.5", mean)
	}
}

func TestStore_Prune(t *testing.T) {
	ctx := context.Background()
	st := New(newTestDB(t).DB)
	_ = st.Insert(ctx,
		Sample{DeviceID: "a", Metric: "m", TS: 100, Value: 1, N: 1},
		Sample{DeviceID: "a", Metric: "m", TS: 5000, Value: 2, N: 1},
		Sample{DeviceID: "b", Metric: "m", TS: 100, Value: 3, N: 1},
	)
	// Per-device prune older than 1000: removes a@100 only.
	n, err := st.Prune(ctx, "a", 1000)
	if err != nil || n != 1 {
		t.Fatalf("prune a = (%d,%v), want 1", n, err)
	}
	if got, _ := st.Query(ctx, "a", "m", 0, 10000, 0); len(got) != 1 || got[0].TS != 5000 {
		t.Errorf("a should keep only ts=5000: %+v", got)
	}
	// All-device prune older than 1000: removes b@100.
	n, _ = st.Prune(ctx, "", 1000)
	if n != 1 {
		t.Errorf("all-prune removed %d, want 1 (b@100)", n)
	}
	if devs, _ := st.Devices(ctx); len(devs) != 1 || devs[0] != "a" {
		t.Errorf("Devices() = %v, want [a]", devs)
	}
}

func TestStore_PersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "persist.db")
	d1, err := db.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := New(d1.DB).Insert(ctx, Sample{DeviceID: "s", Metric: "temperature", TS: 1000, Value: 21.5, N: 3}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_ = d1.Close()
	// Reopen the same file - a restart. The sample must still be there.
	d2, err := db.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = d2.Close() })
	got, err := New(d2.DB).Query(ctx, "s", "temperature", 0, 2000, 0)
	if err != nil || len(got) != 1 || got[0].Value != 21.5 || got[0].N != 3 {
		t.Fatalf("sample did not survive restart: %+v err=%v", got, err)
	}
}

func TestConfigStore_DefaultsOverridesClampPersist(t *testing.T) {
	ctx := context.Background()
	d := newTestDB(t)
	cs := NewConfigStore(d.DB)
	if err := cs.Load(ctx); err != nil {
		t.Fatalf("load: %v", err)
	}
	// Default resolution.
	if g := cs.Get("unknown"); g.Interval != DefaultInterval || g.Retention != DefaultRetention {
		t.Fatalf("default = %+v, want %v/%v", g, DefaultInterval, DefaultRetention)
	}
	// Override + clamp: 99999s interval clamps to 15 min; retention 7 days.
	if err := cs.Set(ctx, "sen-1", 99999, int64((7 * 24 * time.Hour).Seconds())); err != nil {
		t.Fatalf("set: %v", err)
	}
	g := cs.Get("sen-1")
	if g.Interval != MaxFineInterval {
		t.Errorf("interval clamp = %v, want %v", g.Interval, MaxFineInterval)
	}
	if g.Retention != 7*24*time.Hour {
		t.Errorf("retention = %v, want 7d", g.Retention)
	}
	// Hourly is allowed unclamped; 0 clears back to default.
	_ = cs.Set(ctx, "sen-2", 3600, 0)
	if cs.Get("sen-2").Interval != time.Hour {
		t.Errorf("hourly not preserved: %v", cs.Get("sen-2").Interval)
	}
	if cs.Get("sen-2").Retention != DefaultRetention {
		t.Errorf("retention 0 should inherit default, got %v", cs.Get("sen-2").Retention)
	}
	// Persistence across a fresh Load (same DB).
	cs2 := NewConfigStore(d.DB)
	_ = cs2.Load(ctx)
	if cs2.Get("sen-1").Interval != MaxFineInterval {
		t.Errorf("override did not persist across reload")
	}
}

func TestRecorder_AveragesAndFlushesAtBoundary(t *testing.T) {
	ctx := context.Background()
	d := newTestDB(t)
	st := New(d.DB)
	cs := NewConfigStore(d.DB)
	_ = cs.Load(ctx)
	_ = cs.Set(ctx, "sen-1", 60, 0) // 60 s interval

	var nowMs atomic.Int64
	nowMs.Store(1_000_000) // arbitrary base
	rec := NewRecorder(RecorderConfig{Store: st, Config: cs, Now: func() time.Time { return time.UnixMilli(nowMs.Load()) }})

	base := nowMs.Load()
	bucketStart := (base / 60000) * 60000
	// Three readings in the same 60 s bucket: 20, 22, 24 -> mean 22.
	for i, v := range []float64{20, 22, 24} {
		rec.ingest(ctx, reading{deviceID: "sen-1", metric: "temperature", value: v, at: time.UnixMilli(base + int64(i)*1000)})
	}
	// Not yet flushed (window not elapsed).
	if got, _ := st.Query(ctx, "sen-1", "temperature", 0, base+120000, 0); len(got) != 0 {
		t.Fatalf("should not flush before the boundary: %+v", got)
	}
	// Advance past the bucket end and flush.
	nowMs.Store(bucketStart + 61000)
	rec.flushElapsed(ctx)
	got, _ := st.Query(ctx, "sen-1", "temperature", 0, base+120000, 0)
	if len(got) != 1 || got[0].TS != bucketStart || math.Abs(got[0].Value-22) > 1e-9 || got[0].N != 3 {
		t.Fatalf("flushed sample wrong: %+v", got)
	}
}

func TestRecorder_NoReadingNoSample(t *testing.T) {
	ctx := context.Background()
	d := newTestDB(t)
	st := New(d.DB)
	cs := NewConfigStore(d.DB)
	_ = cs.Load(ctx)
	rec := NewRecorder(RecorderConfig{Store: st, Config: cs, Now: time.Now})
	rec.flushElapsed(ctx) // no buckets at all
	rec.flushAll(ctx)
	if devs, _ := st.Devices(ctx); len(devs) != 0 {
		t.Fatalf("no readings should write no samples, got devices %v", devs)
	}
}

func TestRecorder_RecordNeverBlocks(t *testing.T) {
	d := newTestDB(t)
	cs := NewConfigStore(d.DB)
	_ = cs.Load(context.Background())
	rec := NewRecorder(RecorderConfig{Store: New(d.DB), Config: cs, Buffer: 4}) // tiny buffer, no consumer running
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			rec.Record("s", "m", float64(i), time.Now())
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record blocked (storage must never throttle the live path)")
	}
	if rec.dropped.Load() == 0 {
		t.Errorf("expected drops with a full buffer + no consumer")
	}
}

func TestRecorder_PruneRetention(t *testing.T) {
	ctx := context.Background()
	d := newTestDB(t)
	st := New(d.DB)
	cs := NewConfigStore(d.DB)
	_ = cs.Load(ctx)
	_ = cs.Set(ctx, "sen-1", 0, 3600) // retention 1 hour

	var nowMs atomic.Int64
	nowMs.Store(10 * 3600 * 1000) // t = 10h
	rec := NewRecorder(RecorderConfig{Store: st, Config: cs, Now: func() time.Time { return time.UnixMilli(nowMs.Load()) }})

	old := nowMs.Load() - 2*3600*1000 // 2h old (past 1h retention)
	fresh := nowMs.Load() - 600*1000  // 10 min old
	_ = st.Insert(ctx,
		Sample{DeviceID: "sen-1", Metric: "m", TS: old, Value: 1, N: 1},
		Sample{DeviceID: "sen-1", Metric: "m", TS: fresh, Value: 2, N: 1},
	)
	rec.pruneRetention(ctx)
	got, _ := st.Query(ctx, "sen-1", "m", 0, nowMs.Load(), 0)
	if len(got) != 1 || got[0].TS != fresh {
		t.Fatalf("retention should drop the 2h-old sample, kept: %+v", got)
	}
}
