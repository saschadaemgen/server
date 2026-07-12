// Package protectmonitor is the single process-lifetime poller for UniFi
// Protect environmental sensors. It polls the Integration API on a bounded
// ticker into a snapshot that feeds BOTH the Device Center (read the
// snapshot instead of N per-request upstream calls) AND the logic engine
// (a read-only "protect:" Source driver, one RunBinding per run).
//
// The monitor never holds a *protectapi.Client directly: it re-reads the
// current source through a provider on every cycle, so a credential change
// (settings save) or the enabled/disabled gate is picked up for free and
// it never races the server's hot-swapped client field. It is read-only -
// there is no path here that writes anything to the controller.
package protectmonitor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"carvilon.local/server/internal/engine"
	"carvilon.local/server/internal/protectapi"
)

// SensorSource is the read surface the monitor polls; *protectapi.Client
// satisfies it. An interface so tests inject a fake and the monitor never
// reaches into the HTTP transport.
type SensorSource interface {
	ListSensors(ctx context.Context) ([]protectapi.Sensor, error)
}

// MetricInfo describes one readable value a UP-Sense exposes, for the
// engine channel set and the designer catalog. Token is the stable
// address segment ("temperature"); the physical ref is
// "protect:<sensorID>:<token>".
type MetricInfo struct {
	Token string
	Label string
	Unit  string
	Kind  engine.Kind
}

// metricCatalog is the fixed UP-Sense readout set, in display order. Float
// metrics carry a unit; the boolean states carry none. leak/tamper are
// derived from event timestamps with the same freshness window the display
// labels use (see protectapi.Sensor.LeakActive).
var metricCatalog = []MetricInfo{
	{Token: "temperature", Label: "Temperature", Unit: "°C", Kind: engine.Float},
	{Token: "humidity", Label: "Humidity", Unit: "%", Kind: engine.Float},
	{Token: "illuminance", Label: "Illuminance", Unit: "lx", Kind: engine.Float},
	{Token: "battery", Label: "Battery", Unit: "%", Kind: engine.Float},
	{Token: "motion", Label: "Motion", Kind: engine.Bool},
	{Token: "contact", Label: "Contact", Kind: engine.Bool},
	{Token: "leak", Label: "Leak", Kind: engine.Bool},
	{Token: "tamper", Label: "Tamper", Kind: engine.Bool},
}

// MetricCatalog returns the fixed UP-Sense readout descriptors (copy).
func MetricCatalog() []MetricInfo {
	return append([]MetricInfo(nil), metricCatalog...)
}

// readings computes the currently-present readouts of a sensor as engine
// Values, keyed by metric token. A metric absent from the map means the
// sensor does not report it (ok=false from the accessor), so a bound
// source holds its last value rather than snapping to a misleading zero.
func readings(s protectapi.Sensor, now time.Time) map[string]engine.Value {
	out := make(map[string]engine.Value, len(metricCatalog))
	if v, ok := s.TemperatureValue(); ok {
		out["temperature"] = engine.FloatVal(v)
	}
	if v, ok := s.HumidityValue(); ok {
		out["humidity"] = engine.FloatVal(v)
	}
	if v, ok := s.LightValue(); ok {
		out["illuminance"] = engine.FloatVal(v)
	}
	if v, ok := s.BatteryValue(); ok {
		out["battery"] = engine.FloatVal(v)
	}
	if v, ok := s.MotionActive(); ok {
		out["motion"] = engine.BoolVal(v)
	}
	if v, ok := s.OpenedActive(); ok {
		out["contact"] = engine.BoolVal(v)
	}
	if active, present := s.LeakActive(now); present {
		out["leak"] = engine.BoolVal(active)
	}
	if active, present := s.TamperActive(now); present {
		out["tamper"] = engine.BoolVal(active)
	}
	return out
}

// metricLabel returns the display label for a metric token ("" if unknown).
func metricLabel(token string) string {
	for _, m := range metricCatalog {
		if m.Token == token {
			return m.Label
		}
	}
	return ""
}

// metricKind returns the engine Kind for a metric token.
func metricKind(token string) (engine.Kind, bool) {
	for _, m := range metricCatalog {
		if m.Token == token {
			return m.Kind, true
		}
	}
	return 0, false
}

// Snapshot is an immutable view of the last poll. Callers MUST treat the
// slice/map as read-only - each poll replaces them wholesale, never
// mutates in place.
type Snapshot struct {
	Sensors []protectapi.Sensor          // last good list (empty when unconfigured)
	ByID    map[string]protectapi.Sensor // same, keyed by sensor ID
	OK      bool                         // the last poll succeeded
	Polled  bool                         // at least one poll has been attempted
}

// Config configures a Monitor.
type Config struct {
	// Source returns the current sensor source, or nil when Protect is not
	// configured/enabled. Re-read on every poll so a settings save or a
	// disable takes effect without restarting the monitor.
	Source func() SensorSource
	// Interval between polls (default 5s).
	Interval time.Duration
	// Now is the clock (default time.Now); injectable for tests and for the
	// leak/tamper freshness window.
	Now func() time.Time
	// Log sink (default slog.Default()).
	Log *slog.Logger
}

// Monitor is the persistent poller. Safe for concurrent use.
type Monitor struct {
	source   func() SensorSource
	interval time.Duration
	now      func() time.Time
	log      *slog.Logger

	mu       sync.RWMutex
	snap     Snapshot
	bindings map[*RunBinding]struct{}
}

// pollTimeout bounds a single ListSensors call so a wedged NVR cannot stall
// the poll loop indefinitely (the client's own default is 15s).
const pollTimeout = 10 * time.Second

// New builds a Monitor. Call Run to start polling.
func New(cfg Config) *Monitor {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Monitor{
		source:   cfg.Source,
		interval: cfg.Interval,
		now:      cfg.Now,
		log:      cfg.Log,
		snap:     Snapshot{ByID: map[string]protectapi.Sensor{}},
		bindings: map[*RunBinding]struct{}{},
	}
}

// Run polls until ctx is cancelled. The first poll is immediate so the
// snapshot is warm within a cycle, not after the first interval.
func (m *Monitor) Run(ctx context.Context) {
	m.pollOnce(ctx)
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.pollOnce(ctx)
		}
	}
}

// pollOnce fetches the sensor list, stores the snapshot, and pushes changed
// readouts to every active RunBinding. Delivery callbacks (which route into
// the engine's async queue and take the engine lock) run OUTSIDE the
// monitor lock, so cb -> EnqueueInput never nests under m.mu.
func (m *Monitor) pollOnce(ctx context.Context) {
	src := m.currentSource()
	if src == nil {
		// Protect not configured/enabled: clear the snapshot so the UI
		// shows nothing and bound ports get no values.
		m.mu.Lock()
		m.snap = Snapshot{ByID: map[string]protectapi.Sensor{}, OK: false, Polled: true}
		m.mu.Unlock()
		return
	}
	cctx, cancel := context.WithTimeout(ctx, pollTimeout)
	sensors, err := src.ListSensors(cctx)
	cancel()
	if err != nil {
		// Keep the last good sensors but mark the snapshot stale; push
		// nothing so ports hold their last value. Log only on the
		// transition to failure (or the very first poll), not every cycle,
		// so a briefly-unreachable NVR does not spam the admin System Log.
		// protectapi redacts the host from every error, so logging leaks
		// nothing.
		m.mu.Lock()
		transition := m.snap.OK || !m.snap.Polled
		m.snap.OK = false
		m.snap.Polled = true
		m.mu.Unlock()
		if transition {
			m.log.Warn("protect sensor poll failed", "err", err)
		}
		return
	}

	now := m.now()
	m.mu.RLock()
	recovered := m.snap.Polled && !m.snap.OK
	m.mu.RUnlock()
	if recovered {
		m.log.Info("protect sensor poll recovered", "sensors", len(sensors))
	}
	byID := make(map[string]protectapi.Sensor, len(sensors))
	vals := make(map[string]engine.Value, len(sensors)*4)
	for _, s := range sensors {
		byID[s.ID] = s
		for token, v := range readings(s, now) {
			vals[s.ID+":"+token] = v
		}
	}

	type job struct {
		cb func(engine.Value)
		v  engine.Value
	}
	var jobs []job
	m.mu.Lock()
	m.snap = Snapshot{Sensors: sensors, ByID: byID, OK: true, Polled: true}
	for b := range m.bindings {
		b.mu.Lock()
		for addr, cb := range b.subs {
			nv, ok := vals[addr]
			if !ok {
				continue // sensor/metric gone this round; hold last value
			}
			if old, seen := b.last[addr]; seen && old == nv {
				continue // unchanged; skip (level semantics)
			}
			b.last[addr] = nv
			jobs = append(jobs, job{cb: cb, v: nv})
		}
		b.mu.Unlock()
	}
	m.mu.Unlock()

	for _, j := range jobs {
		j.cb(j.v)
	}
}

func (m *Monitor) currentSource() SensorSource {
	if m.source == nil {
		return nil
	}
	// A typed-nil source (e.g. a nil *protectapi.Client wrapped in the
	// interface) must read as "not configured".
	s := m.source()
	if s == nil {
		return nil
	}
	if c, ok := s.(*protectapi.Client); ok && c == nil {
		return nil
	}
	return s
}

// Snapshot returns the last poll's view (read-only; see Snapshot).
func (m *Monitor) Snapshot() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.snap
}

// currentValue reads a single channel's current value from the snapshot.
func (m *Monitor) currentValue(addr string) (engine.Value, bool) {
	id, token, ok := splitAddr(addr)
	if !ok {
		return engine.Value{}, false
	}
	m.mu.RLock()
	s, found := m.snap.ByID[id]
	m.mu.RUnlock()
	if !found {
		return engine.Value{}, false
	}
	v, ok := readings(s, m.now())[token]
	return v, ok
}

// splitAddr splits a "<sensorID>:<metric>" channel address on the LAST
// colon (the metric token never contains one; a sensor ID should not, but
// splitting on the last colon is safe if it ever did).
func splitAddr(addr string) (id, token string, ok bool) {
	i := strings.LastIndex(addr, ":")
	if i <= 0 || i == len(addr)-1 {
		return "", "", false
	}
	return addr[:i], addr[i+1:], true
}

// channelsFor enumerates the engine channels for a sensor set: one per
// present readout, addressed "<sensorID>:<metric>", labelled with the
// sensor name.
func channelsFor(sensors []protectapi.Sensor, now time.Time) []engine.Channel {
	var out []engine.Channel
	for _, s := range sensors {
		present := readings(s, now)
		name := s.DisplayName()
		for _, mi := range metricCatalog {
			if _, ok := present[mi.Token]; !ok {
				continue
			}
			out = append(out, engine.Channel{
				Address: s.ID + ":" + mi.Token,
				Label:   strings.TrimSpace(name + " " + mi.Label),
				Kind:    mi.Kind,
			})
		}
	}
	return out
}

// NewRunBinding returns a per-run engine Source over the current snapshot.
// Register it under engine.PrefixProtect and Close it on teardown.
func (m *Monitor) NewRunBinding() *RunBinding {
	b := &RunBinding{
		m:    m,
		subs: map[string]func(engine.Value){},
		last: map[string]engine.Value{},
	}
	m.mu.Lock()
	b.chans = channelsFor(m.snap.Sensors, m.now())
	m.bindings[b] = struct{}{}
	m.mu.Unlock()
	return b
}

// RunBinding is a per-run read-only engine.Source backed by the monitor's
// snapshot. Its channel set is fixed at creation (a sensor discovered
// mid-run is not added to a running graph, matching sysmetrics).
type RunBinding struct {
	m     *Monitor
	chans []engine.Channel

	mu     sync.Mutex
	subs   map[string]func(engine.Value)
	last   map[string]engine.Value
	closed bool
}

// Channels lists the sensor readout channels available to this run.
func (b *RunBinding) Channels() []engine.Channel {
	return append([]engine.Channel(nil), b.chans...)
}

// Subscribe binds cb to a channel address and delivers the current value
// immediately (the Source contract's "current level"). Errors on an unknown
// or already-bound address.
func (b *RunBinding) Subscribe(addr string, cb func(engine.Value)) error {
	known := false
	for _, c := range b.chans {
		if c.Address == addr {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("protectmonitor: unknown channel %q", addr)
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return fmt.Errorf("protectmonitor: binding closed")
	}
	if _, dup := b.subs[addr]; dup {
		b.mu.Unlock()
		return fmt.Errorf("protectmonitor: channel %q already bound", addr)
	}
	b.subs[addr] = cb
	b.mu.Unlock()

	if v, ok := b.m.currentValue(addr); ok {
		b.mu.Lock()
		if !b.closed {
			b.last[addr] = v
		}
		b.mu.Unlock()
		cb(v)
	}
	return nil
}

// Close detaches the binding from the monitor. Idempotent.
func (b *RunBinding) Close() error {
	b.m.mu.Lock()
	delete(b.m.bindings, b)
	b.m.mu.Unlock()
	b.mu.Lock()
	b.closed = true
	b.subs = map[string]func(engine.Value){}
	b.mu.Unlock()
	return nil
}

// Compile-time contracts: a read-only Source and a Closer, never a Sink.
var (
	_ engine.Source = (*RunBinding)(nil)
	_ io.Closer     = (*RunBinding)(nil)
)
