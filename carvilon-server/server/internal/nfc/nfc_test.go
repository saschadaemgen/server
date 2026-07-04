package nfc

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"carvilon.local/server/internal/engine"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestFormatUID(t *testing.T) {
	cases := []struct {
		uid  []byte
		want string
	}{
		{[]byte{0xD6, 0x45, 0x90, 0x3B}, "D6:45:90:3B"},
		{[]byte{0x04, 0xA3, 0x1B, 0x2C, 0x5D, 0x80, 0x00}, "04:A3:1B:2C:5D:80:00"},
		{nil, ""},
	}
	for _, tc := range cases {
		if got := formatUID(tc.uid); got != tc.want {
			t.Errorf("formatUID(% X) = %q, want %q", tc.uid, got, tc.want)
		}
	}
}

func TestSplitAddr(t *testing.T) {
	cases := []struct {
		addr   string
		id, ch string
		ok     bool
	}{
		{"i2c-1:uid", "i2c-1", "uid", true},
		{"i2c-13:present", "i2c-13", "present", true},
		{"i2c-1", "", "", false},
		{":uid", "", "", false},
		{"i2c-1:", "", "", false},
	}
	for _, tc := range cases {
		id, ch, ok := splitAddr(tc.addr)
		if id != tc.id || ch != tc.ch || ok != tc.ok {
			t.Errorf("splitAddr(%q) = (%q, %q, %v), want (%q, %q, %v)", tc.addr, id, ch, ok, tc.id, tc.ch, tc.ok)
		}
	}
}

// TestTagStateApply pins the debounce state machine: a resting tag
// fires once, removal needs missThreshold blank rounds, the UID level
// survives removal, and a re-presented same tag fires only the present
// edge (the engine dedups an unchanged Text level anyway).
func TestTagStateApply(t *testing.T) {
	var s tagState
	step := func(uid string, found bool) []tagEvent { return s.apply(uid, found) }

	// Arrival: UID first, then the present rising edge.
	evs := step("D6:45:90:3B", true)
	if len(evs) != 2 || evs[0].ch != chanUID || evs[0].v != engine.TextVal("D6:45:90:3B") ||
		evs[1].ch != chanPresent || evs[1].v != engine.BoolVal(true) {
		t.Fatalf("arrival events = %+v", evs)
	}
	// Resting tag: no further events, round after round.
	for i := 0; i < 5; i++ {
		if evs := step("D6:45:90:3B", true); len(evs) != 0 {
			t.Fatalf("resting tag fired again: %+v", evs)
		}
	}
	// One blank round is flutter, not removal.
	if evs := step("", false); len(evs) != 0 {
		t.Fatalf("single miss dropped the tag: %+v", evs)
	}
	// The tag is back before the threshold: still no event.
	if evs := step("D6:45:90:3B", true); len(evs) != 0 {
		t.Fatalf("flutter re-detection fired: %+v", evs)
	}
	// Removal: missThreshold consecutive blanks drop present once.
	if evs := step("", false); len(evs) != 0 {
		t.Fatalf("first miss fired: %+v", evs)
	}
	evs = step("", false)
	if len(evs) != 1 || evs[0].ch != chanPresent || evs[0].v != engine.BoolVal(false) {
		t.Fatalf("removal events = %+v", evs)
	}
	if evs := step("", false); len(evs) != 0 {
		t.Fatalf("absent tag kept firing: %+v", evs)
	}
	// Same tag again: the UID level is unchanged, only present rises.
	evs = step("D6:45:90:3B", true)
	if len(evs) != 1 || evs[0].ch != chanPresent || evs[0].v != engine.BoolVal(true) {
		t.Fatalf("re-presentation events = %+v", evs)
	}
	// A different tag while the first still rests: UID changes, present stays.
	evs = step("04:A3:1B:2C:5D:80:00", true)
	if len(evs) != 1 || evs[0].ch != chanUID || evs[0].v != engine.TextVal("04:A3:1B:2C:5D:80:00") {
		t.Fatalf("tag swap events = %+v", evs)
	}
}

func TestClassify(t *testing.T) {
	okReader := func(dev string) (detectedReader, error) {
		return detectedReader{info: ReaderInfo{ID: "i2c-1", Model: "PN532", Firmware: "1.6"}}, nil
	}
	eacces := func(dev string) (detectedReader, error) {
		return detectedReader{}, &fs.PathError{Op: "open", Path: dev, Err: fs.ErrPermission}
	}
	noReader := func(dev string) (detectedReader, error) {
		return detectedReader{}, errors.New("nfc: not a pn532")
	}
	log := discardLogger()

	if st, rs := classify(nil, okReader, log); st != Unavailable || rs != nil {
		t.Errorf("no devices: (%v, %v), want (Unavailable, nil)", st, rs)
	}
	if st, rs := classify([]string{"/dev/i2c-1"}, eacces, log); st != Forbidden || rs != nil {
		t.Errorf("all EACCES: (%v, %v), want (Forbidden, nil)", st, rs)
	}
	if st, rs := classify([]string{"/dev/i2c-1"}, noReader, log); st != Unavailable || rs != nil {
		t.Errorf("bus without reader: (%v, %v), want (Unavailable, nil)", st, rs)
	}
	// One forbidden bus plus one with a reader: the reader wins.
	probe := func(dev string) (detectedReader, error) {
		if dev == "/dev/i2c-0" {
			return eacces(dev)
		}
		return okReader(dev)
	}
	st, rs := classify([]string{"/dev/i2c-0", "/dev/i2c-1"}, probe, log)
	if st != Available || len(rs) != 1 {
		t.Errorf("mixed buses: (%v, %d readers), want (Available, 1)", st, len(rs))
	}
}

// TestClassifyLogsFailedBuses pins the observability fix: every bus
// whose probe fails must leave one visible Info line - a silent failure
// looked exactly like "no hardware" on the RPi. Permission failures get
// their own line (the aggregated EACCES warning never fires on a mixed
// host where another bus has a reader).
func TestClassifyLogsFailedBuses(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	probe := func(dev string) (detectedReader, error) {
		if dev == "/dev/i2c-0" {
			return detectedReader{}, &fs.PathError{Op: "open", Path: dev, Err: fs.ErrPermission}
		}
		return detectedReader{}, errors.New("nfc: await ack for command 0x02: nfc: pn532 not ready in time")
	}
	if st, _ := classify([]string{"/dev/i2c-0", "/dev/i2c-1"}, probe, log); st != Forbidden {
		t.Fatalf("status = %v, want Forbidden", st)
	}
	out := buf.String()
	if !strings.Contains(out, "no pn532 on bus") || !strings.Contains(out, "/dev/i2c-1") {
		t.Errorf("failed bus not logged: %q", out)
	}
	if !strings.Contains(out, "bus not accessible") || !strings.Contains(out, "/dev/i2c-0") {
		t.Errorf("EACCES bus not logged per-bus: %q", out)
	}
}

// mockReaders installs a fake detection result and restores the package
// cache (probeFn, status, readers, claims) afterwards, mirroring
// sysmetrics' mockMetrics.
func mockReaders(t *testing.T, readers []detectedReader) {
	t.Helper()
	prev := probeFn
	probeFn = func() (Status, []detectedReader) { return Available, readers }
	Probe(discardLogger())
	t.Cleanup(func() {
		probeFn = prev
		mu.Lock()
		status = Unavailable
		detected = nil
		inUse = map[string]bool{}
		mu.Unlock()
	})
}

// fakeTagReader scripts Poll results for the driver tests.
type fakeTagReader struct {
	mu     sync.Mutex
	uid    []byte
	found  bool
	err    error
	closed bool
	polled chan struct{} // when set, signalled once per Poll
}

func (f *fakeTagReader) set(uid []byte, found bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uid, f.found, f.err = uid, found, err
}

func (f *fakeTagReader) Poll() ([]byte, bool, error) {
	f.mu.Lock()
	uid, found, err := f.uid, f.found, f.err
	polled := f.polled
	f.mu.Unlock()
	if polled != nil {
		select {
		case polled <- struct{}{}:
		default:
		}
	}
	return uid, found, err
}

func (f *fakeTagReader) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func TestProbeAvailability(t *testing.T) {
	fake := &fakeTagReader{}
	mockReaders(t, []detectedReader{{
		info: ReaderInfo{ID: "i2c-1", Model: "PN532", Firmware: "1.6"},
		open: func() (tagReader, error) { return fake, nil },
	}})
	if !Enabled() {
		t.Fatal("Enabled() = false after a reader was probed")
	}
	rs := Readers()
	if len(rs) != 1 || rs[0].ID != "i2c-1" {
		t.Fatalf("Readers() = %+v, want one i2c-1", rs)
	}
	// Probe fills the full channel refs centrally.
	if rs[0].UIDChannel != "nfc:i2c-1:uid" || rs[0].PresentChannel != "nfc:i2c-1:present" {
		t.Errorf("channel refs = %q / %q", rs[0].UIDChannel, rs[0].PresentChannel)
	}
}

func TestProbeForbidden(t *testing.T) {
	prev := probeFn
	probeFn = func() (Status, []detectedReader) { return Forbidden, nil }
	Probe(discardLogger())
	t.Cleanup(func() {
		probeFn = prev
		mu.Lock()
		status = Unavailable
		detected = nil
		mu.Unlock()
	})
	if Enabled() {
		t.Error("Forbidden must not count as enabled")
	}
	if len(Readers()) != 0 {
		t.Error("Forbidden must expose no readers")
	}
}

// TestProbeDefaultNoHardware runs the real platform probe: on the dev
// machine it must simply report nothing, never panic.
func TestProbeDefaultNoHardware(t *testing.T) {
	prev := probeFn
	t.Cleanup(func() {
		probeFn = prev
		mu.Lock()
		status = Unavailable
		detected = nil
		mu.Unlock()
	})
	probeFn = platformProbe
	st := Probe(discardLogger())
	if st == Available && len(Readers()) == 0 {
		t.Error("Available without readers")
	}
	if !Enabled() && len(Readers()) != 0 {
		t.Error("readers exposed while disabled")
	}
}

// testDriver builds a Driver directly around a fake reader, bypassing
// Subscribe's claim/open/goroutine start, so step() drives the value
// path deterministically - no ticker, no wall clock (the sysmetrics
// readOnce test pattern).
func testDriver(fake *fakeTagReader) (*Driver, *runReader) {
	d := &Driver{
		readers:  map[string]*runReader{},
		interval: pollInterval,
		done:     make(chan struct{}),
		log:      discardLogger(),
	}
	r := &runReader{
		info: ReaderInfo{ID: "i2c-1", Model: "PN532", Firmware: "1.6"},
		dev:  fake,
		subs: map[string]func(engine.Value){},
	}
	d.readers["i2c-1"] = r
	return d, r
}

func TestDriverStepDeliversTagEvents(t *testing.T) {
	fake := &fakeTagReader{}
	d, r := testDriver(fake)
	var got []engine.Value
	r.subs[chanUID] = func(v engine.Value) { got = append(got, v) }
	r.subs[chanPresent] = func(v engine.Value) { got = append(got, v) }

	d.step(r) // no tag: nothing
	if len(got) != 0 {
		t.Fatalf("events without tag: %+v", got)
	}
	fake.set([]byte{0xD6, 0x45, 0x90, 0x3B}, true, nil)
	d.step(r) // arrival: uid then present
	want := []engine.Value{engine.TextVal("D6:45:90:3B"), engine.BoolVal(true)}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("arrival events = %+v, want %+v", got, want)
	}
	d.step(r) // resting: once means once
	d.step(r)
	if len(got) != 2 {
		t.Fatalf("resting tag re-fired: %+v", got)
	}
	fake.set(nil, false, nil)
	d.step(r) // one miss: flutter guard
	if len(got) != 2 {
		t.Fatalf("single miss fired: %+v", got)
	}
	d.step(r) // second miss: present falls
	if len(got) != 3 || got[2] != engine.BoolVal(false) {
		t.Fatalf("removal events = %+v", got)
	}
}

// TestDriverStepReplaysLevelsToLateSubscriber pins the bind-time race
// fix: a channel subscribed after the poller already latched a resting
// tag gets the current levels replayed on the next round - otherwise
// the second-bound node of a reader would silently miss a tag that
// rests on the module since run start (its edge never re-fires).
func TestDriverStepReplaysLevelsToLateSubscriber(t *testing.T) {
	fake := &fakeTagReader{}
	d, r := testDriver(fake)
	r.started = true // poller counts as live; Subscribe takes the replay path
	var uids, present []engine.Value
	r.subs[chanUID] = func(v engine.Value) { uids = append(uids, v) }

	// The resting tag is latched while only uid is subscribed.
	fake.set([]byte{0xD6, 0x45, 0x90, 0x3B}, true, nil)
	d.step(r)
	if len(uids) != 1 {
		t.Fatalf("uid events = %+v", uids)
	}
	// present binds late (BindGraph reached its node after the first poll).
	if err := d.Subscribe("i2c-1:present", func(v engine.Value) { present = append(present, v) }); err != nil {
		t.Fatalf("late Subscribe: %v", err)
	}
	// A transient poll error must keep the replay pending, not eat it.
	fake.set(nil, false, errors.New("bus glitch"))
	d.step(r)
	if len(present) != 0 {
		t.Fatalf("replay delivered on an error round: %+v", present)
	}
	fake.set([]byte{0xD6, 0x45, 0x90, 0x3B}, true, nil)
	d.step(r)
	if len(present) != 1 || present[0] != engine.BoolVal(true) {
		t.Fatalf("late subscriber replay = %+v, want [true]", present)
	}
	// Replay happens once; the resting tag stays quiet afterwards.
	d.step(r)
	if len(present) != 1 || len(uids) != 1 {
		t.Fatalf("replay repeated: uid=%+v present=%+v", uids, present)
	}
}

func TestDriverStepSkipsUnsubscribedChannel(t *testing.T) {
	fake := &fakeTagReader{}
	d, r := testDriver(fake)
	var got []engine.Value
	r.subs[chanPresent] = func(v engine.Value) { got = append(got, v) }
	fake.set([]byte{0xD6, 0x45, 0x90, 0x3B}, true, nil)
	d.step(r)
	if len(got) != 1 || got[0] != engine.BoolVal(true) {
		t.Fatalf("present-only subscription got %+v", got)
	}
}

func TestDriverStepErrorAging(t *testing.T) {
	fake := &fakeTagReader{}
	d, r := testDriver(fake)
	var got []engine.Value
	r.subs[chanPresent] = func(v engine.Value) { got = append(got, v) }
	fake.set([]byte{0xD6, 0x45, 0x90, 0x3B}, true, nil)
	d.step(r)
	if len(got) != 1 {
		t.Fatalf("arrival events = %+v", got)
	}
	// Transient errors keep the state: no removal, no re-fire.
	fake.set(nil, false, errors.New("bus glitch"))
	for i := 0; i < errThreshold-1; i++ {
		d.step(r)
	}
	if len(got) != 1 {
		t.Fatalf("transient errors changed state: %+v", got)
	}
	// The threshold round drops the present level.
	d.step(r)
	if len(got) != 2 || got[1] != engine.BoolVal(false) {
		t.Fatalf("error aging events = %+v", got)
	}
	// Recovery with the same resting tag: a fresh detection edge.
	fake.set([]byte{0xD6, 0x45, 0x90, 0x3B}, true, nil)
	d.step(r)
	if len(got) != 3 || got[2] != engine.BoolVal(true) {
		t.Fatalf("recovery events = %+v", got)
	}
}

func TestSubscribeRejectsUnknownAddresses(t *testing.T) {
	fake := &fakeTagReader{}
	mockReaders(t, []detectedReader{{
		info: ReaderInfo{ID: "i2c-1", Model: "PN532", Firmware: "1.6"},
		open: func() (tagReader, error) { return fake, nil },
	}})
	d, err := NewDriver()
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	defer d.Close()
	for _, addr := range []string{"i2c-1:volume", "i2c-2:uid", "uid", ""} {
		if err := d.Subscribe(addr, func(engine.Value) {}); err == nil {
			t.Errorf("Subscribe(%q) accepted", addr)
		}
	}
}

func TestDriverChannels(t *testing.T) {
	fake := &fakeTagReader{}
	mockReaders(t, []detectedReader{{
		info: ReaderInfo{ID: "i2c-1", Model: "PN532", Firmware: "1.6"},
		open: func() (tagReader, error) { return fake, nil },
	}})
	d, err := NewDriver()
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	defer d.Close()
	chans := d.Channels()
	if len(chans) != 2 {
		t.Fatalf("Channels() = %+v, want 2", chans)
	}
	byAddr := map[string]engine.Kind{}
	for _, c := range chans {
		byAddr[c.Address] = c.Kind
	}
	if byAddr["i2c-1:uid"] != engine.Text {
		t.Errorf("uid channel kind = %v, want Text", byAddr["i2c-1:uid"])
	}
	if byAddr["i2c-1:present"] != engine.Bool {
		t.Errorf("present channel kind = %v, want Bool", byAddr["i2c-1:present"])
	}
}

// TestReaderExclusiveAcrossDrivers pins the claim registry: /dev/i2c
// has no kernel exclusivity, so a second run binding the same reader
// must fail loudly at bind time - and succeed again after the first
// run released it.
func TestReaderExclusiveAcrossDrivers(t *testing.T) {
	fake := &fakeTagReader{}
	mockReaders(t, []detectedReader{{
		info: ReaderInfo{ID: "i2c-1", Model: "PN532", Firmware: "1.6"},
		open: func() (tagReader, error) { return fake, nil },
	}})
	d1, err := NewDriver()
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	d1.interval = time.Millisecond
	if err := d1.Subscribe("i2c-1:uid", func(engine.Value) {}); err != nil {
		t.Fatalf("first Subscribe: %v", err)
	}
	d2, err := NewDriver()
	if err != nil {
		t.Fatalf("NewDriver (second): %v", err)
	}
	if err := d2.Subscribe("i2c-1:present", func(engine.Value) {}); err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("second driver Subscribe err = %v, want claim conflict", err)
	}
	_ = d2.Close()
	if err := d1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !fake.closed {
		t.Error("Close did not close the reader device")
	}
	// The claim is released: a fresh run can bind the reader again.
	d3, err := NewDriver()
	if err != nil {
		t.Fatalf("NewDriver (third): %v", err)
	}
	d3.interval = time.Millisecond
	if err := d3.Subscribe("i2c-1:uid", func(engine.Value) {}); err != nil {
		t.Fatalf("Subscribe after release: %v", err)
	}
	_ = d3.Close()
}

func TestCloseIdempotent(t *testing.T) {
	fake := &fakeTagReader{}
	mockReaders(t, []detectedReader{{
		info: ReaderInfo{ID: "i2c-1", Model: "PN532", Firmware: "1.6"},
		open: func() (tagReader, error) { return fake, nil },
	}})
	d, err := NewDriver()
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	d.interval = time.Millisecond
	if err := d.Subscribe("i2c-1:uid", func(engine.Value) {}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := d.Subscribe("i2c-1:present", func(engine.Value) {}); err == nil {
		t.Error("Subscribe after Close accepted")
	}
}

// TestDriverLiveGoroutine runs the real poll goroutine at a tiny
// interval and synchronizes on the callbacks - the -race smoke test for
// the subscribe/poll/close interplay. No bare sleeps: every wait is a
// bounded select.
func TestDriverLiveGoroutine(t *testing.T) {
	fake := &fakeTagReader{polled: make(chan struct{}, 1)}
	mockReaders(t, []detectedReader{{
		info: ReaderInfo{ID: "i2c-1", Model: "PN532", Firmware: "1.6"},
		open: func() (tagReader, error) { return fake, nil },
	}})
	d, err := NewDriver()
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	d.interval = time.Millisecond
	uids := make(chan engine.Value, 8)
	present := make(chan engine.Value, 8)
	if err := d.Subscribe("i2c-1:uid", func(v engine.Value) { uids <- v }); err != nil {
		t.Fatalf("Subscribe uid: %v", err)
	}
	if err := d.Subscribe("i2c-1:present", func(v engine.Value) { present <- v }); err != nil {
		t.Fatalf("Subscribe present: %v", err)
	}
	// Wait for the poller to be live, then put a tag on the reader.
	select {
	case <-fake.polled:
	case <-time.After(5 * time.Second):
		t.Fatal("poller never polled")
	}
	fake.set([]byte{0x04, 0xA3, 0x1B, 0x2C, 0x5D, 0x80, 0x00}, true, nil)
	// The channels may first see their no-tag levels (the late-subscribe
	// replay) before the arrival edges - wait for the tag values.
	await := func(name string, ch <-chan engine.Value, want engine.Value) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case v := <-ch:
				if v == want {
					return
				}
			case <-deadline:
				t.Fatalf("%s value %+v never arrived", name, want)
			}
		}
	}
	await("uid", uids, engine.TextVal("04:A3:1B:2C:5D:80:00"))
	await("present", present, engine.BoolVal(true))
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
