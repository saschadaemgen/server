// Package telegramdriver is the engine adapter-layer driver for the
// "telegram:" namespace: it turns allowlisted Telegram chats into
// engine Sources and Sinks, exactly like gpio:, sys: and mqtt: - a
// pure addition, the engine is unchanged. It rides on the bot
// manager's in-process Conn (internal/telegrambot); the manager owns
// the single poller, the allowlist gate, and the rate-limited send
// worker, so this driver never performs network I/O itself.
//
// Address grammar (driver-local, after the "telegram:" prefix):
//
//	send:<chatid>[#slot]  sink.   Bool: sends the configured message
//	                      (ChannelConfig "message") on a rising edge.
//	                      Text: sends the written text verbatim.
//	cmd:<word>[#slot]     source, Bool. Pulses true->false when an
//	                      allowlisted chat sends the word (trimmed,
//	                      case-insensitive).
//	chat:<chatid>[#slot]  source, Text. The raw incoming text of one
//	                      allowlisted chat.
//
// Everything after '#' is a slot discriminator the editor appends
// (the node id): the run path enforces one physical channel per node,
// but two send blocks to the same chat - doorbell AND alarm - are the
// normal case. The slot keeps the refs unique per block; routing
// ignores it. By the same token, routing fans out to ALL matching
// channels: two blocks with the same command word both pulse.
//
// Determinism is preserved the same way as the other drivers: an
// incoming message never touches the eval path - the manager's
// listener callback routes into the engine's async EnqueueInput queue
// (wired by BindGraph), landing at the next tick. Lock order: the
// inbound mutex (inMu) is held while calling the engine callback
// (inMu -> engine lock); the tick path runs engine lock -> Write(mu)
// -> Conn.Send(manager stateMu). mu and inMu are never nested and
// neither is taken with the engine lock wanted afterwards, so there
// is no cycle. Note sinkChannel level semantics: a condition that is
// already true when the run starts IS a rising edge (message at run
// start), and identical consecutive texts are deduplicated by the
// engine - both are engine-wide conventions, not driver bugs.
package telegramdriver

import (
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"carvilon.local/server/internal/engine"
	"carvilon.local/server/internal/telegrambot"
)

// pulseDuration is how long a command source holds true. It must span
// at least one engine tick (the designer runs at 100ms) so the pulse
// is sampled - generously, because a stalled ticker (GC, SQLite
// checkpoint on a loaded RPi) drains true AND false in one tick and
// the engine's last-wins staging would swallow the command entirely.
// A repeated command inside the window retriggers (the pulse extends).
const pulseDuration = 500 * time.Millisecond

// Role constants of the address grammar.
const (
	RoleSend = "send"
	RoleCmd  = "cmd"
	RoleChat = "chat"
)

// Addr is one parsed driver-local address.
type Addr struct {
	Role   string
	ChatID int64  // send / chat
	Word   string // cmd (trimmed, matched case-insensitively)
}

// ParseAddr validates and splits a driver-local address (the part
// after "telegram:"). The '#slot' suffix (everything after the LAST
// '#' - the editor appends exactly one) is stripped; cutting at the
// last one means a stray '#' inside a command word stays part of the
// word instead of silently binding a shorter command.
func ParseAddr(addr string) (Addr, error) {
	base := addr
	if i := strings.LastIndex(addr, "#"); i >= 0 {
		base = addr[:i]
	}
	role, rest, ok := strings.Cut(base, ":")
	if !ok {
		return Addr{}, fmt.Errorf("invalid telegram address %q (want send:<chat-id>, cmd:<wort> or chat:<chat-id>)", addr)
	}
	switch role {
	case RoleSend, RoleChat:
		if strings.TrimSpace(rest) == "" {
			// The most common editor mistake gets a plain message, not
			// grammar internals: the block simply has no chat picked.
			return Addr{}, fmt.Errorf("kein Chat gewählt (Telegram-Block ohne Ziel-Chat)")
		}
		id, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
		if err != nil || id == 0 {
			return Addr{}, fmt.Errorf("invalid telegram chat id %q (want an integer chat id)", rest)
		}
		return Addr{Role: role, ChatID: id}, nil
	case RoleCmd:
		word := strings.TrimSpace(rest)
		if word == "" {
			return Addr{}, fmt.Errorf("kein Befehlswort gesetzt (Telegram-Befehl-Block)")
		}
		return Addr{Role: RoleCmd, Word: word}, nil
	default:
		return Addr{}, fmt.Errorf("unknown telegram address role %q (want send, cmd or chat)", role)
	}
}

// route is one bound channel's routing entry.
type route struct {
	addr string // full driver-local address, slot included
	a    Addr
	kind engine.Kind
}

// Driver exposes a run's telegram channels over the bot manager's
// Conn. One instance per run; the channel set is fixed at
// construction (built from the run's graph). Close detaches the
// manager listener and invalidates pending pulse timers.
type Driver struct {
	conn telegrambot.Conn
	log  *slog.Logger

	chans  []engine.Channel
	kinds  map[string]engine.Kind
	routes []route

	// mu: sink-side state (configured messages, closed flag for
	// Write). Held only for map access, never across Conn.Send's
	// enqueue result handling - Send itself never calls back here.
	mu     sync.Mutex
	msgs   map[string]string // send addr -> configured message (Bool sinks)
	closed bool

	// inMu: inbound routing (subscribed callbacks, pulse generations,
	// its own closed flag). Held across the engine callback - see the
	// package comment for the lock-order argument. The generation
	// counter makes a late pulse-end timer harmless: it only fires
	// cb(false) when no newer command retriggered and the driver is
	// still open.
	inMu     sync.Mutex
	cbs      map[string]func(engine.Value)
	pulseGen map[string]int
	inClosed bool

	removeOnce     sync.Once
	removeListener func()
}

// NewDriver builds a driver for the given channels (each a validated
// telegram address + Kind derived from its graph node - see
// buildTelegramChannels in the run handler). conn is the bot
// manager's in-process surface; it must be non-nil (the caller checks
// the bot is running before constructing the driver).
func NewDriver(conn telegrambot.Conn, channels []engine.Channel, log *slog.Logger) (*Driver, error) {
	if log == nil {
		log = slog.Default()
	}
	d := &Driver{
		conn:     conn,
		log:      log.With("component", "telegram-driver"),
		chans:    append([]engine.Channel(nil), channels...),
		kinds:    make(map[string]engine.Kind, len(channels)),
		msgs:     map[string]string{},
		cbs:      map[string]func(engine.Value){},
		pulseGen: map[string]int{},
	}
	for _, c := range channels {
		a, err := ParseAddr(c.Address)
		if err != nil {
			return nil, fmt.Errorf("telegramdriver: channel %q: %w", c.Address, err)
		}
		d.kinds[c.Address] = c.Kind
		d.routes = append(d.routes, route{addr: c.Address, a: a, kind: c.Kind})
	}
	return d, nil
}

// Channels lists the chats/commands this run binds, as engine channels.
func (d *Driver) Channels() []engine.Channel { return d.chans }

// ConfigureInput is a no-op: command and text sources have no per-line
// options. It exists so the driver satisfies Configurable uniformly.
func (d *Driver) ConfigureInput(addr string, cfg engine.ChannelConfig) error { return nil }

// ConfigureOutput records a Bool send sink's fixed message before the
// first Write. An empty message on a Bool sink is a bind error - the
// block would silently send nothing, so fail loudly in the editor
// instead. Text sinks send the written value and take no options.
func (d *Driver) ConfigureOutput(addr string, cfg engine.ChannelConfig) error {
	kind, ok := d.kinds[addr]
	if !ok {
		return fmt.Errorf("telegramdriver: unknown channel %q", addr)
	}
	if kind != engine.Bool {
		return nil
	}
	msg := strings.TrimSpace(cfg["message"])
	if msg == "" {
		return fmt.Errorf("telegramdriver: channel %q: Nachricht fehlt (Param message)", addr)
	}
	d.mu.Lock()
	d.msgs[addr] = msg
	d.mu.Unlock()
	return nil
}

// Subscribe wires an engine callback to a cmd or chat source channel.
// The manager listener is attached on the first subscription; routing
// then fans incoming messages out to every matching bound channel.
func (d *Driver) Subscribe(addr string, cb func(engine.Value)) error {
	r, ok := d.routeFor(addr)
	if !ok {
		return fmt.Errorf("telegramdriver: unknown channel %q", addr)
	}
	if r.a.Role == RoleSend {
		return fmt.Errorf("telegramdriver: channel %q is send-only (bind it to a sink)", addr)
	}
	d.inMu.Lock()
	if d.inClosed {
		d.inMu.Unlock()
		return fmt.Errorf("telegramdriver: driver closed")
	}
	d.cbs[addr] = cb
	attach := d.removeListener == nil
	if attach {
		// Reserve the slot under inMu so a concurrent Subscribe cannot
		// attach twice; the actual AddListener happens outside the lock.
		d.removeListener = func() {}
	}
	d.inMu.Unlock()
	if attach {
		remove := d.conn.AddListener(d.onMessage)
		d.inMu.Lock()
		closed := d.inClosed
		if !closed {
			d.removeListener = remove
		}
		d.inMu.Unlock()
		if closed {
			// Close raced the unlocked AddListener and consumed the
			// placeholder: detach immediately or the manager would hold
			// the dead callback for the process lifetime.
			remove()
		}
	}
	return nil
}

// onMessage routes one accepted (allowlisted, fresh) incoming message
// to every matching bound channel: command words pulse, chat channels
// carry the text. Runs on the manager's poller goroutine; the engine
// callbacks only stage into the tick queue.
func (d *Driver) onMessage(m telegrambot.Msg) {
	d.inMu.Lock()
	defer d.inMu.Unlock()
	if d.inClosed {
		return
	}
	for _, r := range d.routes {
		cb := d.cbs[r.addr]
		if cb == nil {
			continue
		}
		switch r.a.Role {
		case RoleCmd:
			if strings.EqualFold(strings.TrimSpace(m.Text), r.a.Word) {
				d.pulseGen[r.addr]++
				gen := d.pulseGen[r.addr]
				cb(engine.BoolVal(true))
				addr := r.addr
				time.AfterFunc(pulseDuration, func() { d.endPulse(addr, gen) })
			}
		case RoleChat:
			if m.ChatID == r.a.ChatID {
				cb(engine.TextVal(m.Text))
			}
		}
	}
}

// endPulse drops a command source back to false - unless a newer
// command retriggered the pulse (generation moved on) or the run is
// tearing down. time.Timer.Stop alone cannot give that guarantee (the
// timer goroutine may already be running), the generation check can.
func (d *Driver) endPulse(addr string, gen int) {
	d.inMu.Lock()
	defer d.inMu.Unlock()
	if d.inClosed || d.pulseGen[addr] != gen {
		return
	}
	if cb := d.cbs[addr]; cb != nil {
		cb(engine.BoolVal(false))
	}
}

// Write stages an engine output value for delivery. Called from
// inside a tick (single-threaded, under the engine lock), so it only
// resolves the message and hands it to the manager's non-blocking
// send queue - all HTTP and rate limiting happen in the manager's
// worker, off the tick goroutine. Send refusals (throttle queue full,
// chat revoked mid-run, bot stopped) are logged and dropped, never
// propagated into the tick.
func (d *Driver) Write(addr string, v engine.Value) error {
	r, ok := d.routeFor(addr)
	if !ok {
		return fmt.Errorf("telegramdriver: unknown channel %q", addr)
	}
	if r.a.Role != RoleSend {
		return fmt.Errorf("telegramdriver: channel %q is receive-only", addr)
	}
	var text string
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil // run tearing down; drop
	}
	switch v.Kind {
	case engine.Bool:
		if !v.B {
			d.mu.Unlock()
			return nil // only the rising edge sends
		}
		text = d.msgs[addr]
	case engine.Text:
		text = v.S
	}
	d.mu.Unlock()
	if text == "" {
		return nil // empty text: nothing to send (Telegram rejects it)
	}
	if err := d.conn.Send(r.a.ChatID, text); err != nil {
		d.log.Debug("telegram send refused; dropping", "chat", r.a.ChatID, "err", err)
	}
	return nil
}

// Close detaches the manager listener and invalidates pending pulse
// timers. Idempotent; runs after the final tick (run teardown), so it
// never overlaps a Write.
func (d *Driver) Close() error {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	d.inMu.Lock()
	d.inClosed = true
	remove := d.removeListener
	d.removeListener = nil
	d.inMu.Unlock()
	if remove != nil {
		d.removeOnce.Do(remove)
	}
	return nil
}

func (d *Driver) routeFor(addr string) (route, bool) {
	if _, ok := d.kinds[addr]; !ok {
		return route{}, false
	}
	for _, r := range d.routes {
		if r.addr == addr {
			return r, true
		}
	}
	return route{}, false
}

// Compile-time contract checks: the driver is a Source, a Sink, a
// Configurable (sink message), and a Closer.
var (
	_ engine.Source       = (*Driver)(nil)
	_ engine.Sink         = (*Driver)(nil)
	_ engine.Configurable = (*Driver)(nil)
	_ io.Closer           = (*Driver)(nil)
)
