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
