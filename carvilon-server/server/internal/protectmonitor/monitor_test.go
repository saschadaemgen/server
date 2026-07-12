package protectmonitor

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"carvilon.local/server/internal/engine"
	"carvilon.local/server/internal/protectapi"
)

// fakeSource is an injectable SensorSource for the monitor tests.
type fakeSource struct {
	mu      sync.Mutex
	sensors []protectapi.Sensor
	err     error
	calls   int
}

func (f *fakeSource) ListSensors(context.Context) ([]protectapi.Sensor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return append([]protectapi.Sensor(nil), f.sensors...), nil
}

func (f *fakeSource) set(sensors []protectapi.Sensor, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sensors, f.err = sensors, err
}

// mkSensor builds a protectapi.Sensor from JSON (flexVal fields decode via
// their own UnmarshalJSON, so the tolerant reading matches production).
func mkSensor(t *testing.T, js string) protectapi.Sensor {
	t.Helper()
	var s protectapi.Sensor
	if err := json.Unmarshal([]byte(js), &s); err != nil {
		t.Fatalf("unmarshal sensor: %v", err)
	}
	return s
}

// recorder captures delivered engine values for a channel.
type recorder struct {
	mu   sync.Mutex
	vals []engine.Value
}

func (r *recorder) cb(v engine.Value) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.vals = append(r.vals, v)
}

func (r *recorder) last() (engine.Value, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.vals) == 0 {
		return engine.Value{}, false
	}
	return r.vals[len(r.vals)-1], true
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.vals)
}

func newTestMonitor(fake *fakeSource) *Monitor {
	now := time.UnixMilli(1_700_000_000_000)
	return New(Config{
		Source: func() SensorSource { return fake },
		Now:    func() time.Time { return now },
	})
}

func TestMonitor_SnapshotAfterPoll(t *testing.T) {
	fake := &fakeSource{}
	fake.set([]protectapi.Sensor{
		mkSensor(t, `{"id":"sen-1","name":"Keller","state":"CONNECTED","stats":{"temperature":{"value":21.5},"humidity":{"value":48}}}`),
	}, nil)
	m := newTestMonitor(fake)

	m.pollOnce(context.Background())
	snap := m.Snapshot()
	if !snap.OK || !snap.Polled {
		t.Fatalf("snapshot OK=%v Polled=%v, want both true", snap.OK, snap.Polled)
	}
	if len(snap.Sensors) != 1 || snap.ByID["sen-1"].DisplayName() != "Keller" {
		t.Fatalf("snapshot sensors wrong: %+v", snap.Sensors)
	}
}

func TestRunBinding_ChannelsAndDelivery(t *testing.T) {
	fake := &fakeSource{}
	fake.set([]protectapi.Sensor{
		mkSensor(t, `{"id":"sen-1","name":"Keller","state":"CONNECTED","isMotionDetected":true,"stats":{"temperature":{"value":21.5}}}`),
	}, nil)
	m := newTestMonitor(fake)
	m.pollOnce(context.Background())

	b := m.NewRunBinding()
	defer b.Close()

	// Channels enumerate present readouts with the right kind.
	var haveTemp, haveMotion bool
	for _, c := range b.Channels() {
		switch c.Address {
		case "sen-1:temperature":
			haveTemp = c.Kind == engine.Float
		case "sen-1:motion":
			haveMotion = c.Kind == engine.Bool
		}
	}
	if !haveTemp || !haveMotion {
		t.Fatalf("channels missing temp/motion: %+v", b.Channels())
	}

	// Subscribe delivers the current value immediately.
	rec := &recorder{}
	if err := b.Subscribe("sen-1:temperature", rec.cb); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if v, ok := rec.last(); !ok || v.F != 21.5 {
		t.Fatalf("immediate delivery = %+v ok=%v, want 21.5", v, ok)
	}

	// A changed value on the next poll is delivered.
	fake.set([]protectapi.Sensor{
		mkSensor(t, `{"id":"sen-1","name":"Keller","state":"CONNECTED","isMotionDetected":true,"stats":{"temperature":{"value":22.0}}}`),
	}, nil)
	m.pollOnce(context.Background())
	if v, _ := rec.last(); v.F != 22.0 {
		t.Fatalf("after change = %v, want 22.0", v.F)
	}

	// An unchanged value does not re-deliver (level semantics).
	before := rec.count()
	m.pollOnce(context.Background())
	if rec.count() != before {
		t.Fatalf("unchanged value re-delivered (%d -> %d)", before, rec.count())
	}
}

func TestRunBinding_SubscribeErrors(t *testing.T) {
	fake := &fakeSource{}
	fake.set([]protectapi.Sensor{
		mkSensor(t, `{"id":"sen-1","state":"CONNECTED","stats":{"temperature":{"value":21.5}}}`),
	}, nil)
	m := newTestMonitor(fake)
	m.pollOnce(context.Background())
	b := m.NewRunBinding()
	defer b.Close()

	if err := b.Subscribe("sen-1:nope", func(engine.Value) {}); err == nil {
		t.Errorf("unknown channel should error")
	}
	if err := b.Subscribe("sen-1:temperature", func(engine.Value) {}); err != nil {
		t.Fatalf("first subscribe: %v", err)
	}
	if err := b.Subscribe("sen-1:temperature", func(engine.Value) {}); err == nil {
		t.Errorf("double subscribe should error")
	}
}

func TestMonitor_SourceNilClears(t *testing.T) {
	m := New(Config{Source: func() SensorSource { return nil }})
	m.pollOnce(context.Background())
	snap := m.Snapshot()
	if snap.OK || !snap.Polled || len(snap.Sensors) != 0 {
		t.Fatalf("unconfigured snapshot = %+v, want empty+not-ok+polled", snap)
	}
}

func TestMonitor_PollErrorHoldsLastAndPushesNothing(t *testing.T) {
	fake := &fakeSource{}
	fake.set([]protectapi.Sensor{
		mkSensor(t, `{"id":"sen-1","state":"CONNECTED","stats":{"temperature":{"value":21.5}}}`),
	}, nil)
	m := newTestMonitor(fake)
	m.pollOnce(context.Background())

	b := m.NewRunBinding()
	defer b.Close()
	rec := &recorder{}
	_ = b.Subscribe("sen-1:temperature", rec.cb)
	before := rec.count()

	fake.set(nil, errors.New("protectapi: request failed: timeout"))
	m.pollOnce(context.Background())

	snap := m.Snapshot()
	if snap.OK {
		t.Errorf("snapshot should be stale after a poll error")
	}
	if len(snap.Sensors) != 1 {
		t.Errorf("last good sensors should be retained, got %d", len(snap.Sensors))
	}
	if rec.count() != before {
		t.Errorf("a poll error should push nothing (%d -> %d)", before, rec.count())
	}
}

func TestMonitor_OnReadingTapEmitsAllReadings(t *testing.T) {
	fake := &fakeSource{}
	fake.set([]protectapi.Sensor{
		mkSensor(t, `{"id":"sen-1","state":"CONNECTED","isMotionDetected":true,"stats":{"temperature":{"value":21.5},"humidity":{"value":48}}}`),
	}, nil)

	var mu sync.Mutex
	got := map[string]float64{}
	now := time.UnixMilli(1_700_000_000_000)
	m := New(Config{
		Source:    func() SensorSource { return fake },
		Now:       func() time.Time { return now },
		OnReading: func(id, metric string, v float64, at time.Time) { mu.Lock(); got[id+":"+metric] = v; mu.Unlock() },
	})
	m.pollOnce(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if got["sen-1:temperature"] != 21.5 || got["sen-1:humidity"] != 48 {
		t.Fatalf("float readings not tapped: %+v", got)
	}
	if got["sen-1:motion"] != 1 { // bool -> 1/0
		t.Fatalf("bool motion should tap as 1, got %v", got["sen-1:motion"])
	}
}

func TestMonitor_DevicesReadoutModel(t *testing.T) {
	fake := &fakeSource{}
	fake.set([]protectapi.Sensor{
		mkSensor(t, `{"id":"sen-1","name":"Keller","state":"CONNECTED","isMotionDetected":true,"stats":{"temperature":{"value":21.5},"humidity":{"value":48}}}`),
		mkSensor(t, `{"id":"sen-2","name":"Nothing","state":"CONNECTED"}`), // no readouts -> excluded
	}, nil)
	m := newTestMonitor(fake)
	m.pollOnce(context.Background())

	devs := m.Devices()
	if len(devs) != 1 {
		t.Fatalf("Devices() = %d, want 1 (the readout-less sensor is excluded)", len(devs))
	}
	d := devs[0]
	if d.ID != "sen-1" || d.Name != "Keller" || !d.Online {
		t.Fatalf("device meta wrong: %+v", d)
	}
	got := map[string]Readout{}
	for _, r := range d.Readouts {
		got[r.Token] = r
	}
	temp, ok := got["temperature"]
	if !ok || temp.Channel != "protect:sen-1:temperature" || temp.KindString() != "float" || temp.Unit != "°C" {
		t.Errorf("temperature readout wrong: %+v", temp)
	}
	motion, ok := got["motion"]
	if !ok || motion.Channel != "protect:sen-1:motion" || motion.KindString() != "bool" {
		t.Errorf("motion readout wrong: %+v", motion)
	}
	if _, ok := got["illuminance"]; ok {
		t.Errorf("absent illuminance should not be a readout")
	}
}

func TestRunBinding_CloseDetaches(t *testing.T) {
	fake := &fakeSource{}
	fake.set([]protectapi.Sensor{
		mkSensor(t, `{"id":"sen-1","state":"CONNECTED","stats":{"temperature":{"value":21.5}}}`),
	}, nil)
	m := newTestMonitor(fake)
	m.pollOnce(context.Background())

	b := m.NewRunBinding()
	rec := &recorder{}
	_ = b.Subscribe("sen-1:temperature", rec.cb)
	before := rec.count()
	b.Close()

	fake.set([]protectapi.Sensor{
		mkSensor(t, `{"id":"sen-1","state":"CONNECTED","stats":{"temperature":{"value":99.0}}}`),
	}, nil)
	m.pollOnce(context.Background())
	if rec.count() != before {
		t.Errorf("closed binding still received a value (%d -> %d)", before, rec.count())
	}
}
