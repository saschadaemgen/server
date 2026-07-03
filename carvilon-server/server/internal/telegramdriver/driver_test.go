package telegramdriver

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"carvilon.local/server/internal/engine"
	"carvilon.local/server/internal/telegrambot"
)

// fakeConn records sends and exposes the registered listener, standing
// in for the bot manager's non-blocking in-process surface.
type fakeConn struct {
	mu      sync.Mutex
	sent    []fakeSent
	cb      func(telegrambot.Msg)
	removed bool
	sendErr error
}

type fakeSent struct {
	chatID int64
	text   string
}

func (f *fakeConn) Send(chatID int64, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, fakeSent{chatID: chatID, text: text})
	return nil
}

func (f *fakeConn) AddListener(cb func(telegrambot.Msg)) func() {
	f.mu.Lock()
	f.cb = cb
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		f.removed = true
		f.mu.Unlock()
	}
}

// push delivers a message the way the manager's poller would.
func (f *fakeConn) push(m telegrambot.Msg) {
	f.mu.Lock()
	cb := f.cb
	f.mu.Unlock()
	if cb != nil {
		cb(m)
	}
}

func (f *fakeConn) sends() []fakeSent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeSent(nil), f.sent...)
}

// recorder captures engine callback values with timestamps (the role
// BindGraph's EnqueueInput closure plays in production).
type recorder struct {
	mu   sync.Mutex
	vals []engine.Value
	ts   []time.Time
}

func (r *recorder) cb(v engine.Value) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.vals = append(r.vals, v)
	r.ts = append(r.ts, time.Now())
}

func (r *recorder) snapshot() []engine.Value {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]engine.Value(nil), r.vals...)
}

// waitFor polls until cond is true or the deadline passes.
func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestDriver(t *testing.T, conn *fakeConn, chans ...engine.Channel) *Driver {
	t.Helper()
	d, err := NewDriver(conn, chans, quietLog())
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestParseAddr(t *testing.T) {
	cases := []struct {
		in      string
		want    Addr
		wantErr bool
	}{
		{in: "send:42#n1", want: Addr{Role: RoleSend, ChatID: 42}},
		{in: "send:-100987#telegramsenden_3", want: Addr{Role: RoleSend, ChatID: -100987}},
		{in: "chat:7", want: Addr{Role: RoleChat, ChatID: 7}},
		{in: "cmd:licht an#n2", want: Addr{Role: RoleCmd, Word: "licht an"}},
		{in: "cmd: licht an ", want: Addr{Role: RoleCmd, Word: "licht an"}},
		// The slot is everything after the LAST '#': a stray '#' in the
		// word stays part of the word instead of binding a shorter one.
		{in: "cmd:alarm#2#n5", want: Addr{Role: RoleCmd, Word: "alarm#2"}},
		{in: "send:abc#n1", wantErr: true}, // non-integer chat id
		{in: "send:0#n1", wantErr: true},   // zero chat id
		{in: "cmd:#n1", wantErr: true},     // empty word
		{in: "cmd:   #n1", wantErr: true},  // whitespace-only word
		{in: "ring:42#n1", wantErr: true},  // unknown role
		{in: "send#n1", wantErr: true},     // no payload
		{in: "42", wantErr: true},          // no role
	}
	for _, c := range cases {
		got, err := ParseAddr(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseAddr(%q) = %+v, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseAddr(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseAddr(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestDriver_CommandPulse(t *testing.T) {
	conn := &fakeConn{}
	addr := "cmd:licht an#n1"
	d := newTestDriver(t, conn, engine.Channel{Address: addr, Label: addr, Kind: engine.Bool})
	rec := &recorder{}
	if err := d.Subscribe(addr, rec.cb); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Matching is trimmed + case-insensitive.
	conn.push(telegrambot.Msg{ChatID: 42, Text: "  Licht AN "})
	if !waitFor(t, 2*time.Second, func() bool { return len(rec.snapshot()) == 2 }) {
		t.Fatalf("pulse values = %v, want [true false]", rec.snapshot())
	}
	vals := rec.snapshot()
	if !vals[0].B || vals[0].Kind != engine.Bool {
		t.Errorf("first value = %+v, want Bool true", vals[0])
	}
	if vals[1].B {
		t.Errorf("second value = %+v, want Bool false", vals[1])
	}

	// A non-matching text does nothing.
	conn.push(telegrambot.Msg{ChatID: 42, Text: "licht aus"})
	time.Sleep(50 * time.Millisecond)
	if n := len(rec.snapshot()); n != 2 {
		t.Errorf("non-matching command produced values: %d, want 2", n)
	}
}

// TestDriver_CommandRetriggerExtends: a repeated command inside the
// pulse window retriggers it - the stale first timer must NOT cut the
// extended pulse short (generation guard).
func TestDriver_CommandRetriggerExtends(t *testing.T) {
	conn := &fakeConn{}
	addr := "cmd:auf#n1"
	d := newTestDriver(t, conn, engine.Channel{Address: addr, Label: addr, Kind: engine.Bool})
	rec := &recorder{}
	if err := d.Subscribe(addr, rec.cb); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	conn.push(telegrambot.Msg{ChatID: 1, Text: "auf"})
	time.Sleep(pulseDuration / 2)
	second := time.Now()
	conn.push(telegrambot.Msg{ChatID: 1, Text: "auf"})

	if !waitFor(t, 2*time.Second, func() bool {
		vals := rec.snapshot()
		return len(vals) > 0 && !vals[len(vals)-1].B
	}) {
		t.Fatalf("pulse never ended: %v", rec.snapshot())
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	// Exactly one false, at the end, no false in between (the stale
	// timer of the first command must have been invalidated).
	for i, v := range rec.vals {
		if !v.B && i != len(rec.vals)-1 {
			t.Fatalf("premature pulse end at %d: %v", i, rec.vals)
		}
	}
	end := rec.ts[len(rec.ts)-1]
	if got := end.Sub(second); got < pulseDuration-50*time.Millisecond {
		t.Errorf("pulse ended %v after the retrigger, want ~%v", got, pulseDuration)
	}
}

// TestDriver_RoutingFanOut: two blocks with the same command word (and
// two text sources on the same chat) BOTH fire - routing is a loop
// over all bound channels, never a keyed map.
func TestDriver_RoutingFanOut(t *testing.T) {
	conn := &fakeConn{}
	cmdA, cmdB := "cmd:licht an#a", "cmd:licht an#b"
	chatA, chatB := "chat:42#a", "chat:42#b"
	d := newTestDriver(t, conn,
		engine.Channel{Address: cmdA, Kind: engine.Bool},
		engine.Channel{Address: cmdB, Kind: engine.Bool},
		engine.Channel{Address: chatA, Kind: engine.Text},
		engine.Channel{Address: chatB, Kind: engine.Text},
	)
	recs := map[string]*recorder{cmdA: {}, cmdB: {}, chatA: {}, chatB: {}}
	for addr, r := range recs {
		if err := d.Subscribe(addr, r.cb); err != nil {
			t.Fatalf("Subscribe %q: %v", addr, err)
		}
	}

	conn.push(telegrambot.Msg{ChatID: 42, Text: "licht an"})
	for _, addr := range []string{cmdA, cmdB} {
		r := recs[addr]
		if !waitFor(t, time.Second, func() bool { return len(r.snapshot()) >= 1 }) {
			t.Errorf("command channel %q never pulsed", addr)
		}
	}
	// The same message is also raw text of chat 42: both text blocks see it.
	for _, addr := range []string{chatA, chatB} {
		vals := recs[addr].snapshot()
		if len(vals) != 1 || vals[0].S != "licht an" {
			t.Errorf("text channel %q values = %v, want [licht an]", addr, vals)
		}
	}
}

func TestDriver_ChatTextRouting(t *testing.T) {
	conn := &fakeConn{}
	addr := "chat:42#n1"
	d := newTestDriver(t, conn, engine.Channel{Address: addr, Kind: engine.Text})
	rec := &recorder{}
	if err := d.Subscribe(addr, rec.cb); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	conn.push(telegrambot.Msg{ChatID: 99, Text: "fremder chat"})
	conn.push(telegrambot.Msg{ChatID: 42, Text: "hallo"})
	if !waitFor(t, time.Second, func() bool { return len(rec.snapshot()) == 1 }) {
		t.Fatalf("values = %v, want exactly the chat-42 text", rec.snapshot())
	}
	if v := rec.snapshot()[0]; v.Kind != engine.Text || v.S != "hallo" {
		t.Errorf("value = %+v, want Text hallo", v)
	}
}

func TestDriver_WriteBoolSendsConfiguredMessage(t *testing.T) {
	conn := &fakeConn{}
	addr := "send:42#n1"
	d := newTestDriver(t, conn, engine.Channel{Address: addr, Kind: engine.Bool})
	if err := d.ConfigureOutput(addr, engine.ChannelConfig{"message": "Es hat geklingelt!"}); err != nil {
		t.Fatalf("ConfigureOutput: %v", err)
	}
	if err := d.Write(addr, engine.BoolVal(true)); err != nil {
		t.Fatalf("Write(true): %v", err)
	}
	if err := d.Write(addr, engine.BoolVal(false)); err != nil {
		t.Fatalf("Write(false): %v", err)
	}
	sent := conn.sends()
	if len(sent) != 1 || sent[0] != (fakeSent{chatID: 42, text: "Es hat geklingelt!"}) {
		t.Errorf("sends = %+v, want exactly the rising-edge message", sent)
	}
}

func TestDriver_WriteTextSendsText(t *testing.T) {
	conn := &fakeConn{}
	addr := "send:42#n1"
	d := newTestDriver(t, conn, engine.Channel{Address: addr, Kind: engine.Text})
	if err := d.ConfigureOutput(addr, nil); err != nil {
		t.Fatalf("ConfigureOutput: %v", err)
	}
	if err := d.Write(addr, engine.TextVal("hallo welt")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := d.Write(addr, engine.TextVal("")); err != nil {
		t.Fatalf("Write empty: %v", err)
	}
	sent := conn.sends()
	if len(sent) != 1 || sent[0].text != "hallo welt" {
		t.Errorf("sends = %+v, want just the non-empty text", sent)
	}
}

func TestDriver_ConfigureOutputRequiresMessage(t *testing.T) {
	conn := &fakeConn{}
	addr := "send:42#n1"
	d := newTestDriver(t, conn, engine.Channel{Address: addr, Kind: engine.Bool})
	if err := d.ConfigureOutput(addr, nil); err == nil {
		t.Error("ConfigureOutput without message on a Bool send sink: want error")
	}
	if err := d.ConfigureOutput(addr, engine.ChannelConfig{"message": "  "}); err == nil {
		t.Error("ConfigureOutput with blank message: want error")
	}
}

func TestDriver_RoleDirectionErrors(t *testing.T) {
	conn := &fakeConn{}
	send, cmd := "send:42#n1", "cmd:auf#n2"
	d := newTestDriver(t, conn,
		engine.Channel{Address: send, Kind: engine.Bool},
		engine.Channel{Address: cmd, Kind: engine.Bool},
	)
	if err := d.Subscribe(send, func(engine.Value) {}); err == nil {
		t.Error("Subscribe on a send channel: want error")
	}
	if err := d.Write(cmd, engine.BoolVal(true)); err == nil {
		t.Error("Write on a cmd channel: want error")
	}
	if err := d.Write("send:7#nope", engine.BoolVal(true)); err == nil {
		t.Error("Write on an unbound address: want error")
	}
	if _, err := NewDriver(conn, []engine.Channel{{Address: "bogus", Kind: engine.Bool}}, quietLog()); err == nil {
		t.Error("NewDriver with an invalid address: want error")
	}
}

// TestDriver_CloseInvalidatesPulseAndDetaches: Close before the pulse
// timer fires must swallow the late cb(false), and the manager
// listener must be removed.
func TestDriver_CloseInvalidatesPulseAndDetaches(t *testing.T) {
	conn := &fakeConn{}
	addr := "cmd:auf#n1"
	d := newTestDriver(t, conn, engine.Channel{Address: addr, Kind: engine.Bool})
	rec := &recorder{}
	if err := d.Subscribe(addr, rec.cb); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	conn.push(telegrambot.Msg{ChatID: 1, Text: "auf"})
	if !waitFor(t, time.Second, func() bool { return len(rec.snapshot()) == 1 }) {
		t.Fatalf("pulse start missing: %v", rec.snapshot())
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	time.Sleep(pulseDuration + 100*time.Millisecond)
	if n := len(rec.snapshot()); n != 1 {
		t.Errorf("late pulse-end reached the engine after Close: %d values", n)
	}
	conn.mu.Lock()
	removed := conn.removed
	conn.mu.Unlock()
	if !removed {
		t.Error("Close did not remove the manager listener")
	}
	// Write after Close is a silent drop, and a second Close is a no-op.
	if err := d.Write("cmd:auf#n1", engine.BoolVal(true)); err == nil {
		t.Error("Write on cmd after close should still be a role error")
	}
	if err := d.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}
