package sysmetrics

import (
	"errors"
	"log/slog"
	"testing"

	"carvilon.local/server/internal/engine"
)

// mockMetrics installs a fixed candidate set (one readable, one failing)
// and probes it, restoring the real platform list afterwards. The failing
// metric proves per-metric detection: only readable ones survive.
func mockMetrics(t *testing.T) {
	t.Helper()
	prev := probeFn
	probeFn = func() []metricDef {
		return []metricDef{
			{addr: "ok", label: "OK metric", unit: "x", read: func() (float64, error) { return 42.5, nil }},
			{addr: "bad", label: "Bad metric", unit: "", read: func() (float64, error) { return 0, errors.New("unreadable") }},
		}
	}
	Probe(slog.Default())
	t.Cleanup(func() {
		probeFn = prev
		mu.Lock()
		available = nil
		mu.Unlock()
	})
}

// TestProbeKeepsReadableOnly: only the metric whose reader succeeds is
// offered; the failing one is dropped.
func TestProbeKeepsReadableOnly(t *testing.T) {
	mockMetrics(t)
	if !Enabled() {
		t.Fatal("expected Enabled with one readable metric")
	}
	ms := Metrics()
	if len(ms) != 1 {
		t.Fatalf("Metrics = %+v, want exactly the readable one", ms)
	}
	if ms[0].Address != "sys:ok" || ms[0].Label != "OK metric" || ms[0].Unit != "x" {
		t.Errorf("Metric = %+v, want {sys:ok, OK metric, x}", ms[0])
	}
}

// TestProbeEmptyWhenNoneReadable: a host with no readable metric (the
// non-Linux stub returns nil) is disabled and offers nothing.
func TestProbeEmptyWhenNoneReadable(t *testing.T) {
	prev := probeFn
	probeFn = func() []metricDef { return nil }
	Probe(slog.Default())
	defer func() {
		probeFn = prev
		mu.Lock()
		available = nil
		mu.Unlock()
	}()
	if Enabled() {
		t.Error("expected disabled with no readable metric")
	}
	if len(Metrics()) != 0 {
		t.Errorf("Metrics = %+v, want empty", Metrics())
	}
}

// TestDriverReadsTypedFloat: the driver reads a metric and feeds its
// callback a typed Float value (the read -> FloatVal -> cb path the poller
// uses). Exercised directly via readOnce so no wall clock is involved.
func TestDriverReadsTypedFloat(t *testing.T) {
	mockMetrics(t)
	d, err := NewDriver()
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	defer d.Close()

	// Channels expose the metric as a Float source channel.
	chans := d.Channels()
	if len(chans) != 1 || chans[0].Address != "ok" || chans[0].Kind != engine.Float {
		t.Fatalf("Channels = %+v, want one Float channel 'ok'", chans)
	}

	// Bind a callback directly (bypassing the poller goroutine) and read.
	var got []engine.Value
	d.subs["ok"] = func(v engine.Value) { got = append(got, v) }
	d.readOnce()
	if len(got) != 1 || got[0].Kind != engine.Float || got[0].F != 42.5 {
		t.Fatalf("callback got %+v, want [FloatVal(42.5)]", got)
	}
}

// TestSubscribeRejectsUnknown: Subscribe errors on a metric the driver
// does not expose (and so never starts a poller for it).
func TestSubscribeRejectsUnknown(t *testing.T) {
	mockMetrics(t)
	d, err := NewDriver()
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	defer d.Close()
	if err := d.Subscribe("nope", func(engine.Value) {}); err == nil {
		t.Error("Subscribe to an unknown metric must error")
	}
}
