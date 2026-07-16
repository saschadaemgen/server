package shellyhistory

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"carvilon.local/server/internal/shellystore"
)

type fakeStore struct {
	mu   sync.Mutex
	devs []shellystore.Device
	err  error
	// calls counts ListActive, to prove the index is cached rather than
	// rebuilt on every single publish.
	calls int
}

func (f *fakeStore) ListActive(context.Context) ([]shellystore.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return append([]shellystore.Device(nil), f.devs...), nil
}

type rec struct {
	device, metric string
	value          float64
	at             time.Time
}

// testClock is a movable clock. A FROZEN clock would silently disable the
// index TTL (now-built is always 0, so the index is eternally fresh) and no
// test could ever reach the refresh or the store-error branch.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// newTap wires a Tap with a collector, and runs its worker until the test ends.
func newTap(t *testing.T, fs *fakeStore) (*Tap, func() []rec) {
	t.Helper()
	tap, _, collect := newTapClock(t, fs)
	return tap, collect
}

func newTapClock(t *testing.T, fs *fakeStore) (*Tap, *testClock, func() []rec) {
	t.Helper()
	var mu sync.Mutex
	var got []rec
	clk := &testClock{t: time.UnixMilli(1700000000000)}
	tap := New(Config{
		Store: fs,
		Record: func(d, m string, v float64, at time.Time) {
			mu.Lock()
			got = append(got, rec{d, m, v, at})
			mu.Unlock()
		},
		Now: clk.now,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tap.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return tap, clk, func() []rec {
		// The worker is async; give it a moment to drain.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			n := len(got)
			mu.Unlock()
			if n > 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		return append([]rec(nil), got...)
	}
}

const at = int64(1700000000123)

func TestTap_Gen2SwitchStatusRecordsEveryMeteredField(t *testing.T) {
	fs := &fakeStore{devs: []shellystore.Device{
		{ID: 7, MAC: "AABBCCDDEEFF", Gen: shellystore.Gen2, Model: "Shelly Plus1PM", MQTTUsername: "shelly-aabbccddeeff"},
	}}
	tap, collect := newTap(t, fs)

	tap.Handle("carvilon/shelly-aabbccddeeff/status/switch:0",
		[]byte(`{"id":0,"output":true,"apower":123.4,"voltage":231.5,"current":0.53,"freq":50,"temperature":{"tC":42.5,"tF":108.5},"aenergy":{"total":9}}`),
		time.UnixMilli(at))

	got := collect()
	want := map[string]float64{
		"sw0_power": 123.4, "sw0_voltage": 231.5, "sw0_current": 0.53, "sw0_freq": 50, "sw0_temp": 42.5,
	}
	if len(got) != len(want) {
		t.Fatalf("recorded %d readings, want %d: %+v", len(got), len(want), got)
	}
	for _, r := range got {
		// The history key must be the MAC-derived id, NOT the rowid and not the
		// address - both of those move and would orphan the history.
		if r.device != "shelly-aabbccddeeff" {
			t.Errorf("device = %q, want shelly-aabbccddeeff", r.device)
		}
		if r.at.UnixMilli() != at {
			t.Errorf("%s at = %d, want the publish time %d", r.metric, r.at.UnixMilli(), at)
		}
		w, ok := want[r.metric]
		if !ok {
			t.Errorf("unexpected metric %q", r.metric)
			continue
		}
		if r.value != w {
			t.Errorf("%s = %v, want %v", r.metric, r.value, w)
		}
	}
}

// A device that reports no voltage must record NO voltage - not 0 V. A zero
// would be indistinguishable from a real reading and would drag a chart's
// average down.
func TestTap_AbsentFieldsAreNotRecordedAsZero(t *testing.T) {
	fs := &fakeStore{devs: []shellystore.Device{
		{ID: 1, MAC: "AABBCCDDEEFF", Gen: shellystore.Gen2, Model: "Shelly Plus1PM", MQTTUsername: "shelly-aabbccddeeff"},
	}}
	tap, collect := newTap(t, fs)
	tap.Handle("carvilon/shelly-aabbccddeeff/status/switch:0",
		[]byte(`{"id":0,"output":false,"apower":0}`), time.UnixMilli(at))

	got := collect()
	if len(got) != 1 {
		t.Fatalf("recorded %+v, want only sw0_power", got)
	}
	if got[0].metric != "sw0_power" || got[0].value != 0 {
		t.Errorf("got %+v, want a real sw0_power=0", got[0])
	}
}

func TestTap_Gen1PowerIsABareNumberOnItsOwnTopic(t *testing.T) {
	fs := &fakeStore{devs: []shellystore.Device{
		{ID: 2, MAC: "112233445566", Gen: shellystore.Gen1, Model: "SHSW-25", MQTTUsername: "shelly-112233445566"},
	}}
	tap, collect := newTap(t, fs)
	tap.Handle("shellies/shelly-112233445566/relay/1/power", []byte("61.85"), time.UnixMilli(at))

	got := collect()
	if len(got) != 1 || got[0].metric != "sw1_power" || got[0].value != 61.85 ||
		got[0].device != "shelly-112233445566" {
		t.Fatalf("got %+v, want shelly-112233445566 sw1_power 61.85", got)
	}
}

func TestTap_Gen1LightStatusPowerFieldIsPowerNotAPower(t *testing.T) {
	fs := &fakeStore{devs: []shellystore.Device{
		{ID: 3, MAC: "0011AABB2233", Gen: shellystore.Gen1, Model: "SHRGBW2", MQTTUsername: "shelly-0011aabb2233"},
	}}
	tap, collect := newTap(t, fs)
	tap.Handle("shellies/shelly-0011aabb2233/color/0/status",
		[]byte(`{"ison":true,"power":8.4,"effect":0,"red":255}`), time.UnixMilli(at))

	got := collect()
	if len(got) != 1 || got[0].metric != "li0_power" || got[0].value != 8.4 {
		t.Fatalf("got %+v, want li0_power 8.4", got)
	}
}

// The device trees carry plenty of traffic that is not a metered value, and a
// Gen1 device must not be parsed with the Gen2 grammar (or the reverse).
func TestTap_IgnoresEverythingElse(t *testing.T) {
	fs := &fakeStore{devs: []shellystore.Device{
		{ID: 1, MAC: "AABBCCDDEEFF", Gen: shellystore.Gen2, Model: "Shelly Plus1PM", MQTTUsername: "shelly-aabbccddeeff"},
		{ID: 2, MAC: "112233445566", Gen: shellystore.Gen1, Model: "SHSW-25", MQTTUsername: "shelly-112233445566"},
	}}
	tap, collect := newTap(t, fs)

	for _, tc := range []struct{ topic, payload string }{
		{"carvilon/shelly-aabbccddeeff/status/input:0", `{"id":0,"state":true}`}, // not metered
		{"carvilon/shelly-aabbccddeeff/rpc", `{"id":0,"method":"Switch.Set"}`},   // a command
		{"carvilon/shelly-aabbccddeeff/status/switch:0", `not json`},             // malformed
		{"shellies/shelly-112233445566/relay/0", "on"},                           // Gen1 state, not power
		{"shellies/shelly-112233445566/relay/0/power", "nope"},                   // unparseable number
		{"carvilon/shelly-112233445566/status/switch:0", `{"apower":5}`},         // Gen1 device, Gen2 grammar+root
		{"carvilon/unknown-device/status/switch:0", `{"apower":5}`},              // not an adopted device
		{"shellies/shelly-112233445566/relay/x/power", "5"},                      // no channel index
		{"carvilon/", `{"apower":5}`},                                            // degenerate topic
	} {
		tap.Handle(tc.topic, []byte(tc.payload), time.UnixMilli(at))
	}
	// Nothing above is a reading; collect() waits out its grace period.
	if got := collect(); len(got) != 0 {
		t.Fatalf("recorded %+v, want nothing", got)
	}
}

// A device with no MAC has no durable history key, so it must be skipped
// rather than recorded under an empty id.
func TestTap_DeviceWithoutMACIsSkipped(t *testing.T) {
	fs := &fakeStore{devs: []shellystore.Device{
		{ID: 9, MAC: "", Gen: shellystore.Gen2, Model: "Shelly Plus1PM", MQTTUsername: "shelly-nomac"},
	}}
	tap, collect := newTap(t, fs)
	tap.Handle("carvilon/shelly-nomac/status/switch:0", []byte(`{"apower":5}`), time.UnixMilli(at))
	if got := collect(); len(got) != 0 {
		t.Fatalf("recorded %+v for a MAC-less device, want nothing", got)
	}
}

// Handle runs on the broker's publish goroutine: it must never block, even
// when the worker is not draining, or it would stall delivery to the live
// subscribers - the path recording may never throttle.
func TestTap_HandleNeverBlocksWhenTheWorkerIsStarved(t *testing.T) {
	tap := New(Config{
		Store:  &fakeStore{},
		Record: func(string, string, float64, time.Time) {},
	})
	// No Run: nothing drains t.in, so the queue fills and then must drop.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10000; i++ {
			tap.Handle("carvilon/shelly-aabbccddeeff/status/switch:0", []byte(`{"apower":1}`), time.Now())
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Handle blocked with a full queue; it must drop instead")
	}
}

// The payload buffer belongs to the broker's packet and may be reused the
// moment Handle returns.
func TestTap_CopiesThePayloadOffTheBrokerPath(t *testing.T) {
	fs := &fakeStore{devs: []shellystore.Device{
		{ID: 1, MAC: "AABBCCDDEEFF", Gen: shellystore.Gen2, Model: "Shelly Plus1PM", MQTTUsername: "shelly-aabbccddeeff"},
	}}
	tap, collect := newTap(t, fs)
	buf := []byte(`{"apower":77.5}`)
	tap.Handle("carvilon/shelly-aabbccddeeff/status/switch:0", buf, time.UnixMilli(at))
	// Scribble over the caller's buffer straight away, as the broker would.
	for i := range buf {
		buf[i] = 'x'
	}
	got := collect()
	if len(got) != 1 || got[0].value != 77.5 {
		t.Fatalf("got %+v, want sw0_power 77.5 from the copied payload", got)
	}
}

func onlyDevice() []shellystore.Device {
	return []shellystore.Device{
		{ID: 1, MAC: "AABBCCDDEEFF", Gen: shellystore.Gen2, Model: "Shelly Plus1PM", MQTTUsername: "shelly-aabbccddeeff"},
	}
}

func (f *fakeStore) storeCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeStore) setErr(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

// Every publish under the device trees hits lookup, so the index must be
// cached rather than re-read from SQLite each time.
func TestTap_IndexIsCachedWithinTheTTL(t *testing.T) {
	fs := &fakeStore{devs: onlyDevice()}
	tap, clk, _ := newTapClock(t, fs)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, ok := tap.lookup(ctx, "carvilon/shelly-aabbccddeeff"); !ok {
			t.Fatal("device should resolve")
		}
		clk.add(indexTTL / 10) // still inside the window
	}
	if got := fs.storeCalls(); got != 1 {
		t.Errorf("ListActive called %d times inside the TTL, want 1", got)
	}
}

// A device adopted after the index was built must start recording once the
// index refreshes - the TTL is the only thing that picks it up.
func TestTap_TTLExpiryPicksUpANewlyAdoptedDevice(t *testing.T) {
	// The TTL is the ONLY thing that notices an adoption, so it bounds how long
	// a just-adopted device silently records nothing. The assertions below are
	// written against indexTTL rather than a literal, so this pins the policy
	// they cannot: it must stay short enough to feel immediate.
	if indexTTL > time.Minute {
		t.Fatalf("indexTTL is %s: a newly adopted device would record nothing for that long", indexTTL)
	}
	fs := &fakeStore{}
	tap, clk, _ := newTapClock(t, fs)
	ctx := context.Background()
	if _, ok := tap.lookup(ctx, "carvilon/shelly-aabbccddeeff"); ok {
		t.Fatal("nothing is adopted yet; the prefix must not resolve")
	}
	fs.mu.Lock()
	fs.devs = onlyDevice()
	fs.mu.Unlock()

	// Inside the TTL the stale index still says "unknown"...
	if _, ok := tap.lookup(ctx, "carvilon/shelly-aabbccddeeff"); ok {
		t.Error("the index should still be the cached one inside the TTL")
	}
	// ...and past it the device appears.
	clk.add(indexTTL + time.Second)
	if _, ok := tap.lookup(ctx, "carvilon/shelly-aabbccddeeff"); !ok {
		t.Error("a device adopted meanwhile must resolve after the TTL expires")
	}
}

// A transient store error must not stop recording: the previous index has to
// survive, and the failed rebuild must not then retry on every single publish.
func TestTap_StoreErrorKeepsTheIndexAndDoesNotHammer(t *testing.T) {
	fs := &fakeStore{devs: onlyDevice()}
	tap, clk, _ := newTapClock(t, fs)
	ctx := context.Background()
	if _, ok := tap.lookup(ctx, "carvilon/shelly-aabbccddeeff"); !ok {
		t.Fatal("device should resolve from the first build")
	}

	fs.setErr(errors.New("database is locked"))
	clk.add(indexTTL + time.Second) // force a rebuild, which now fails
	if _, ok := tap.lookup(ctx, "carvilon/shelly-aabbccddeeff"); !ok {
		t.Fatal("a store error must keep the previous index, not clear it")
	}
	callsAfterFail := fs.storeCalls()

	// The failed rebuild must still have marked the index fresh, or every
	// publish would fire its own SQLite query on the tap worker.
	for i := 0; i < 20; i++ {
		tap.lookup(ctx, "carvilon/shelly-aabbccddeeff")
	}
	if got := fs.storeCalls(); got != callsAfterFail {
		t.Errorf("ListActive called %d more times after a failed rebuild, want 0: a persistent store error must not hammer the store per publish", got-callsAfterFail)
	}

	// And once the store recovers, the next refresh takes effect.
	fs.setErr(nil)
	clk.add(indexTTL + time.Second)
	if _, ok := tap.lookup(ctx, "carvilon/shelly-aabbccddeeff"); !ok {
		t.Error("device should still resolve after the store recovers")
	}
}
