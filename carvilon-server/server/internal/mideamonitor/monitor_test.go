package mideamonitor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"carvilon.local/server/internal/mideaclimate"
	"carvilon.local/server/internal/mideastore"
)

// fakeStore drives the monitor's reconcile loop without any real device I/O:
// Credential always errors, so connect() returns before opening a socket. This
// exercises the map/handle bookkeeping (tick, provisionAsync, Snapshot, Get,
// Refresh) under -race without needing a live TCP device.
type fakeStore struct {
	mu     sync.Mutex
	active []mideastore.Device
}

func (f *fakeStore) setActive(ds []mideastore.Device) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active = ds
}

func (f *fakeStore) ListActive(ctx context.Context) ([]mideastore.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mideastore.Device(nil), f.active...), nil
}

func (f *fakeStore) Get(ctx context.Context, id string) (mideastore.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.active {
		if d.ID == id {
			return d, nil
		}
	}
	return mideastore.Device{}, mideastore.ErrNotFound
}

func (f *fakeStore) Credential(ctx context.Context, id string) ([]byte, []byte, error) {
	return nil, nil, errors.New("fakeStore: no credentials")
}

// TestConcurrentReconcileAndReads drives tick + Snapshot/Get/Refresh + an
// active-set mutation concurrently; -race must stay silent (map/handle
// bookkeeping is fully guarded by m.mu).
func TestConcurrentReconcileAndReads(t *testing.T) {
	fs := &fakeStore{active: []mideastore.Device{
		{ID: "a", DeviceID: 1, Address: "192.0.2.1", State: mideastore.StateActive},
		{ID: "b", DeviceID: 2, Address: "192.0.2.2", State: mideastore.StateActive},
	}}
	m := New(fs, nil, WithInterval(time.Hour)) // Run not used; call tick directly
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 150; j++ {
				m.tick(ctx)
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 300; j++ {
				_ = m.Snapshot()
				_, _ = m.Get("a")
				m.Refresh()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 100; j++ {
			if j%2 == 0 {
				fs.setActive([]mideastore.Device{{ID: "a", DeviceID: 1, Address: "192.0.2.1", State: mideastore.StateActive}})
			} else {
				fs.setActive([]mideastore.Device{
					{ID: "a", DeviceID: 1, Address: "192.0.2.1", State: mideastore.StateActive},
					{ID: "b", DeviceID: 2, Address: "192.0.2.2", State: mideastore.StateActive},
				})
			}
		}
	}()
	wg.Wait()
	// Let any in-flight provisionAsync goroutines drain (they only touch m.mu).
	time.Sleep(50 * time.Millisecond)
}

func TestParseMode(t *testing.T) {
	cases := map[string]mideaclimate.Mode{
		"cool": mideaclimate.ModeCool, "heat": mideaclimate.ModeHeat,
		"dry": mideaclimate.ModeDry, "fan_only": mideaclimate.ModeFanOnly,
		"off": mideaclimate.ModeOff, "auto": mideaclimate.ModeAuto,
		"bogus": mideaclimate.ModeCool,
	}
	for in, want := range cases {
		if got := parseMode(in); got != want {
			t.Errorf("parseMode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseFan(t *testing.T) {
	cases := map[string]mideaclimate.FanMode{
		"low": mideaclimate.FanLow, "mid": mideaclimate.FanMid,
		"high": mideaclimate.FanHigh, "auto": mideaclimate.FanAuto,
		"bogus": mideaclimate.FanAuto,
	}
	for in, want := range cases {
		if got := parseFan(in); got != want {
			t.Errorf("parseFan(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHostOnly(t *testing.T) {
	cases := map[string]string{
		"192.0.2.10":      "192.0.2.10",
		"192.0.2.10:6444": "192.0.2.10",
		"":                "",
	}
	for in, want := range cases {
		if got := hostOnly(in); got != want {
			t.Errorf("hostOnly(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNilSafety: an unconfigured monitor is inert, not a panic.
func TestNilSafety(t *testing.T) {
	var m *Monitor
	if got := m.Snapshot(); got != nil {
		t.Errorf("nil Snapshot = %v, want nil", got)
	}
	if _, ok := m.Get("x"); ok {
		t.Error("nil Get returned ok")
	}
	m.Refresh() // must not panic
	m.Run(context.Background())

	// A monitor with no store returns from Run immediately.
	m2 := New(nil, nil)
	m2.Run(context.Background())
	if got := m2.Snapshot(); len(got) != 0 {
		t.Errorf("empty Snapshot = %v, want empty", got)
	}
}

// The climate controller's own fühler feed the sensor-history recorder, which
// is what makes the stored-history path capability-driven rather than
// UniFi-only. The tap must fire only for PRESENT readings, must never run with
// the monitor lock held (the recorder is free to be slow), and must be a no-op
// when no tap is installed.
func TestEmitReadings_TapsPresentReadoutsOnly(t *testing.T) {
	type got struct {
		id, metric string
		value      float64
	}
	var mu sync.Mutex
	var seen []got
	var m *Monitor
	m = New(&fakeStore{}, nil, WithOnReading(func(id, metric string, v float64, at time.Time) {
		// Re-enter the monitor from inside the callback: if emitReadings held
		// the lock while calling out, this would deadlock the test.
		_ = m.Snapshot()
		mu.Lock()
		seen = append(seen, got{id, metric, v})
		mu.Unlock()
	}))

	// Both readouts present.
	m.devs["dev-a"] = &devState{
		id:         "dev-a",
		hasState:   true,
		last:       mideaclimate.State{DeviceTempC: 21.5, HasTemp: true, OutdoorC: 8.25, HasOutdoor: true},
		lastPollMS: 1700000000000,
	}
	m.emitReadings("dev-a")

	mu.Lock()
	n := len(seen)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("present readouts emitted %d readings, want 2: %+v", n, seen)
	}
	if seen[0].metric != chDeviceTemp || seen[0].value != 21.5 || seen[0].id != "dev-a" {
		t.Errorf("first reading = %+v, want dev-a %s 21.5", seen[0], chDeviceTemp)
	}
	if seen[1].metric != chOutdoor || seen[1].value != 8.25 {
		t.Errorf("second reading = %+v, want %s 8.25", seen[1], chOutdoor)
	}

	// An ABSENT outdoor sensor must not be recorded as a value.
	seen = nil
	m.devs["dev-b"] = &devState{
		id: "dev-b", hasState: true,
		last:       mideaclimate.State{DeviceTempC: 19, HasTemp: true},
		lastPollMS: 1700000000000,
	}
	m.emitReadings("dev-b")
	if len(seen) != 1 || seen[0].metric != chDeviceTemp {
		t.Errorf("absent outdoor should emit nothing: %+v", seen)
	}

	// A device that has never polled has nothing to record.
	seen = nil
	m.devs["dev-c"] = &devState{id: "dev-c"}
	m.emitReadings("dev-c")
	m.emitReadings("missing")
	if len(seen) != 0 {
		t.Errorf("no state should emit nothing: %+v", seen)
	}
}

func TestEmitReadings_NoTapIsNoop(t *testing.T) {
	m := New(&fakeStore{}, nil)
	m.devs["d"] = &devState{id: "d", hasState: true, last: mideaclimate.State{DeviceTempC: 20, HasTemp: true}}
	m.emitReadings("d") // must not panic on a nil onReading
}
