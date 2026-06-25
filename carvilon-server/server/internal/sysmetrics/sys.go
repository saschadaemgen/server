// Package sysmetrics is a runtime-detected system-telemetry driver for the
// engine adapter layer: it exposes Float SOURCES under the "sys:" prefix
// (CPU temperature, load, RAM, disk - read from /proc and /sys with the
// standard library only). It is the first float driver on the T1/Float
// seam - a pure addition, the engine is unchanged. The real readers live
// in the build-tagged Linux file; every other platform degrades to "no
// telemetry". Detection is per metric at runtime: a host offers only the
// metrics it can actually read (a VPS without a thermal zone drops
// CPU-temp). Telemetry is read-only, so this is a Source, never a Sink.
package sysmetrics

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"carvilon.local/server/internal/engine"
)

// metricDef is one telemetry metric: a driver-local address, its display
// label + unit, and a stdlib reader returning the current value. The
// build-tagged platform file supplies the candidate list.
type metricDef struct {
	addr  string
	label string
	unit  string
	read  func() (float64, error)
}

// Metric is the editor-facing description of an available metric, for the
// catalog's data-driven system blocks. Address is the full physical ref
// ("sys:cpu_temp") the graph stores as the channel param.
type Metric struct {
	Address string `json:"address"`
	Label   string `json:"label"`
	Unit    string `json:"unit"`
}

// Driver is a live system-telemetry source. A poller reads each subscribed
// metric on a wall-clock ticker and feeds the value into the engine via
// EnqueueInput, so values land at the next tick, never concurrent with
// eval (determinism preserved, exactly like the GPIO edge handler). One
// instance per run; Close stops the poller.
type Driver struct {
	mu      sync.Mutex
	readers map[string]func() (float64, error) // available metric addr -> reader
	chans   []engine.Channel
	subs    map[string]func(engine.Value) // bound addr -> engine callback
	done    chan struct{}
	started bool
	log     *slog.Logger
}

// pollInterval is how often the poller re-reads the metrics. Telemetry is
// slow-moving; a couple of seconds keeps the card lively without churn.
const pollInterval = 1500 * time.Millisecond

var (
	mu        sync.Mutex
	available []metricDef
	logger    = slog.Default()
	// probeFn is the platform metric list (set in the build-tagged files);
	// tests override it to drive detection without real /proc reads.
	probeFn = platformMetrics
)

// Probe reads each candidate metric once and keeps the ones that succeed,
// caching them for Enabled / Metrics / NewDriver. It logs the available
// set, or stays silent when there is no telemetry (non-Linux / no /proc).
// Call it once at startup. Never blocks or panics.
func Probe(log *slog.Logger) {
	defs := probeFn()
	var ok []metricDef
	for _, d := range defs {
		if _, err := d.read(); err == nil {
			ok = append(ok, d)
		}
	}
	mu.Lock()
	available = ok
	logger = log
	mu.Unlock()
	if len(ok) > 0 {
		addrs := make([]string, len(ok))
		for i, d := range ok {
			addrs[i] = d.addr
		}
		log.Info("system telemetry available", "metrics", addrs)
	}
}

// Enabled reports whether Probe found any readable metric. The catalog and
// run path key the system surface off this.
func Enabled() bool {
	mu.Lock()
	defer mu.Unlock()
	return len(available) > 0
}

// Metrics returns the available metrics (full ref, label, unit) for the
// catalog's data-driven system blocks. Empty on a host without telemetry.
func Metrics() []Metric {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Metric, 0, len(available))
	for _, d := range available {
		out = append(out, Metric{Address: engine.PrefixSys + ":" + d.addr, Label: d.label, Unit: d.unit})
	}
	return out
}

// NewDriver builds a fresh telemetry driver from the probed metrics, for
// one run. The caller registers it under engine.PrefixSys and Close()s it
// on teardown. Errors on a host with no metrics (where Enabled() is false
// and this is never reached).
func NewDriver() (*Driver, error) {
	mu.Lock()
	defs := append([]metricDef(nil), available...)
	log := logger
	mu.Unlock()
	if len(defs) == 0 {
		return nil, errors.New("sysmetrics: no metrics available")
	}
	d := &Driver{
		readers: make(map[string]func() (float64, error), len(defs)),
		subs:    map[string]func(engine.Value){},
		done:    make(chan struct{}),
		log:     log,
	}
	for _, def := range defs {
		d.readers[def.addr] = def.read
		d.chans = append(d.chans, engine.Channel{Address: def.addr, Label: def.label, Kind: engine.Float})
	}
	return d, nil
}

// Channels lists the available metrics as Float source channels.
func (d *Driver) Channels() []engine.Channel { return d.chans }

// Subscribe binds an engine callback to a metric address and starts the
// poller on the first subscription. cb routes to Engine.EnqueueInput, so
// each poll's value lands at the next tick.
func (d *Driver) Subscribe(addr string, cb func(engine.Value)) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.readers[addr]; !ok {
		return fmt.Errorf("sysmetrics: unknown metric %q", addr)
	}
	d.subs[addr] = cb
	if !d.started {
		d.started = true
		go d.poll()
	}
	return nil
}

// poll re-reads the subscribed metrics on the ticker until Close. The
// first read is immediate so the card shows a value within a frame, not
// after the first interval.
func (d *Driver) poll() {
	d.readOnce()
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		select {
		case <-d.done:
			return
		case <-t.C:
			d.readOnce()
		}
	}
}

// readOnce reads every subscribed metric and feeds the value to its
// callback. It snapshots the subscriptions under d.mu and then reads +
// calls back OUTSIDE the lock, so cb -> EnqueueInput (which takes the
// engine lock) never nests under d.mu. A reader error skips that metric
// for this round (no value pushed) - a transient /proc hiccup is not fatal.
func (d *Driver) readOnce() {
	type job struct {
		read func() (float64, error)
		cb   func(engine.Value)
	}
	d.mu.Lock()
	jobs := make([]job, 0, len(d.subs))
	for addr, cb := range d.subs {
		jobs = append(jobs, job{read: d.readers[addr], cb: cb})
	}
	d.mu.Unlock()
	for _, j := range jobs {
		v, err := j.read()
		if err != nil {
			continue
		}
		j.cb(engine.FloatVal(v))
	}
}

// Close stops the poller. Idempotent.
func (d *Driver) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.started {
		select {
		case <-d.done:
		default:
			close(d.done)
		}
	}
	return nil
}

// Ensure the driver satisfies the engine Source contract (and is a Closer)
// at compile time. It is deliberately NOT a Sink.
var (
	_ engine.Source = (*Driver)(nil)
	_ io.Closer     = (*Driver)(nil)
)
