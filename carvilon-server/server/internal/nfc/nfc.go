// Package nfc is a runtime-detected NFC/RFID tag-reader driver for the
// engine adapter layer: every detected reader exposes a Text source (the
// last read tag UID, canonical "04:A3:..." form) and a Bool source ("tag
// present") under the "nfc:" prefix - a pure addition on the seam, the
// engine is unchanged. The first supported reader model is the PN532
// over I2C (Elechouse-V3-style module); the protocol layer is
// first-party (pn532.go, NXP UM0701-02) - no libnfc, no CGO - and the
// reader-model seam (tagReader) keeps further models pure additions.
// The real /dev/i2c-* access lives in the build-tagged Linux file; every
// other platform degrades to "no readers". Reading only: writing tags,
// tag naming and the Tags page are later tickets.
package nfc

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	"time"

	"carvilon.local/server/internal/engine"
)

// Status is the outcome of probing the host for tag readers.
type Status int

const (
	// Unavailable: no I2C bus, or no PN532 answering on any bus.
	Unavailable Status = iota
	// Forbidden: an I2C bus exists but the process cannot open it
	// (EACCES) - the service user needs to be in the i2c group.
	Forbidden
	// Available: at least one reader answered the firmware probe.
	Available
)

// tagReader is the reader-model seam: one physical tag reader that can
// be polled for the tag currently in its field. The PN532 over I2C is
// the first implementation; further models plug in as new
// implementations without touching the driver above.
type tagReader interface {
	// Poll reports the tag currently in the field, if any.
	Poll() (uid []byte, found bool, err error)
	Close() error
}

// ReaderInfo describes one detected reader for the startup log and the
// editor catalog: a stable ID (the bus it sits on, e.g. "i2c-1"), the
// model and firmware, and the two full channel refs the catalog bakes
// into the reader's palette blocks.
type ReaderInfo struct {
	ID             string
	Model          string
	Firmware       string
	Identity       string // stable registry id, e.g. "nfc:i2c-1"
	UIDChannel     string // full physical ref, e.g. "nfc:i2c-1:uid"
	PresentChannel string // full physical ref, e.g. "nfc:i2c-1:present"
}

// detectedReader is one probed reader: its description plus an opener
// that reopens and configures the device for a run.
type detectedReader struct {
	info ReaderInfo
	open func() (tagReader, error)
}

// The two channels every reader exposes.
const (
	chanUID     = "uid"
	chanPresent = "present"
)

// uidAddr/presentAddr build a reader's driver-local channel addresses
// ("i2c-1:uid"); the full binding refs prefix them with "nfc:"
// (ParsePhysical splits at the first colon only, so the inner colon is
// fine - same pattern as "gpio:gpiochip0:17").
func uidAddr(id string) string     { return id + ":" + chanUID }
func presentAddr(id string) string { return id + ":" + chanPresent }

// splitAddr splits a driver-local channel address into the reader ID
// and the channel name after the last colon.
func splitAddr(addr string) (id, ch string, ok bool) {
	i := strings.LastIndex(addr, ":")
	if i <= 0 || i == len(addr)-1 {
		return "", "", false
	}
	return addr[:i], addr[i+1:], true
}

// formatUID renders a tag UID in the canonical form all later
// comparison/assignment logic keys on: uppercase hex octets,
// colon-separated ("04:A3:1B:2C:5D:80:00").
func formatUID(uid []byte) string {
	var b strings.Builder
	for i, x := range uid {
		if i > 0 {
			b.WriteByte(':')
		}
		fmt.Fprintf(&b, "%02X", x)
	}
	return b.String()
}

// classify turns candidate bus device paths plus a prober into a Status
// and the detected readers. It is platform-independent so the detection
// logic is unit-testable without hardware: probe returns the reader on
// success, a permission error (errors.Is fs.ErrPermission) when the bus
// cannot be opened, or any other error when there is no PN532 on that
// bus. Every probe failure is logged per bus at Info: a bus without a
// responding reader is normal on any host with I2C enabled, but a
// silent failure made a mis-timed exchange on the RPi indistinguishable
// from "no hardware" - never again. Permission failures get their own
// per-bus line too (the aggregated EACCES warning in Probe only fires
// when NO reader was found anywhere, so a mixed host would otherwise
// hide the inaccessible bus). Hosts without /dev/i2c-* stay silent as
// before.
func classify(devs []string, probe func(dev string) (detectedReader, error), log *slog.Logger) (Status, []detectedReader) {
	if len(devs) == 0 {
		return Unavailable, nil
	}
	var found []detectedReader
	forbidden := false
	for _, dev := range devs {
		r, err := probe(dev)
		if err != nil {
			if errors.Is(err, fs.ErrPermission) {
				forbidden = true
				log.Info("nfc probe: bus not accessible", "bus", dev, "err", err)
				continue
			}
			log.Info("nfc probe: no pn532 on bus", "bus", dev, "err", err)
			continue
		}
		found = append(found, r)
	}
	if len(found) > 0 {
		return Available, found
	}
	if forbidden {
		return Forbidden, nil
	}
	return Unavailable, nil
}

var (
	mu       sync.Mutex
	status   Status
	detected []detectedReader
	logger   = slog.Default()
	// probeFn is the platform probe (set in the build-tagged files);
	// tests override it to drive detection without hardware.
	probeFn = platformProbe
)

var (
	obsMu       sync.Mutex
	tagObserver func(readerID, uid string)
)

// SetTagObserver installs a side-channel callback the driver invokes on
// every tag PRESENTATION during a run - a new/changed UID or the same
// tag re-presented - after the tag has already been handed to the
// engine (readerID is the reader's stable registry identity, e.g.
// "nfc:i2c-1"; uid is the canonical tag id). It is a pure addition
// used by the reader registry to record the last-seen tag - it does NOT
// touch the engine tick path, so determinism is unchanged, and it stays
// nil (a no-op) in every context that does not opt in. Pass nil to
// detach. Set once at startup, before any run.
func SetTagObserver(fn func(readerID, uid string)) {
	obsMu.Lock()
	tagObserver = fn
	obsMu.Unlock()
}

// notifyTag delivers a new-tag read to the observer, snapshotting it
// under its own lock so the poll goroutine never calls it while holding
// a driver lock.
func notifyTag(readerID, uid string) {
	obsMu.Lock()
	fn := tagObserver
	obsMu.Unlock()
	if fn != nil {
		fn(readerID, uid)
	}
}

// Probe detects PN532 readers on the I2C buses once at startup and
// caches the result for Enabled / Readers / NewMonitor. It logs the
// outcome - silent when there is no I2C bus at all, one Info line per
// bus whose probe failed, a clear actionable line on EACCES - and never
// blocks or panics. Call it once at startup. The logger is cached
// BEFORE the platform probe runs so the probe itself can report
// per-bus failures through it.
func Probe(log *slog.Logger) Status {
	mu.Lock()
	logger = log
	mu.Unlock()
	st, readers := probeFn()
	for i := range readers {
		readers[i].info.Identity = engine.PrefixNFC + ":" + readers[i].info.ID
		readers[i].info.UIDChannel = engine.PrefixNFC + ":" + uidAddr(readers[i].info.ID)
		readers[i].info.PresentChannel = engine.PrefixNFC + ":" + presentAddr(readers[i].info.ID)
	}
	mu.Lock()
	status = st
	detected = readers
	mu.Unlock()
	switch st {
	case Available:
		ids := make([]string, len(readers))
		for i, r := range readers {
			ids[i] = r.info.ID + " (" + r.info.Model + " " + r.info.Firmware + ")"
		}
		log.Info("nfc readers available", "readers", ids)
	case Forbidden:
		log.Warn("i2c bus detected but access denied (EACCES); add the service user to the i2c group " +
			"(e.g. `sudo usermod -aG i2c <user>`) and restart - NFC disabled for now")
	case Unavailable:
		// No I2C bus / no reader on it (VPS, dev machine): stay silent;
		// nothing NFC surfaces in the catalog or runs.
	}
	return st
}

// Enabled reports whether Probe found a reader. The catalog and the run
// path key the NFC surface off this.
func Enabled() bool {
	mu.Lock()
	defer mu.Unlock()
	return status == Available
}

// Readers returns the detected readers for the catalog's data-driven
// NFC blocks. Empty on a host without readers.
func Readers() []ReaderInfo {
	mu.Lock()
	defer mu.Unlock()
	out := make([]ReaderInfo, 0, len(detected))
	for _, r := range detected {
		out = append(out, r.info)
	}
	return out
}

// currentLogger returns the logger Probe cached (the server logger from
// startup on); the platform probe uses it to report per-bus failures.
func currentLogger() *slog.Logger {
	mu.Lock()
	defer mu.Unlock()
	return logger
}

// pollInterval is the tag poll cycle: fast enough that holding a tag to
// the module feels instant, slow enough to keep the bus quiet. The
// driver copies it into an overridable field so tests can run the real
// goroutine without wall-clock waits.
const pollInterval = 250 * time.Millisecond

// missThreshold is how many consecutive empty polls declare the tag
// gone: one blank round (~250 ms) is RF flutter, two is removal.
const missThreshold = 2

// errThreshold is how many consecutive poll ERRORS it takes to stop
// trusting the last state (~2 s at the 250 ms cycle): a reader that
// died mid-run (unplugged, bus reset) must not hold "tag present" - and
// whatever logic hangs off it - forever.
const errThreshold = 8

// tagState is the per-reader debounce state machine: the same resting
// tag fires ONCE (an edge), not per poll round; removal needs
// missThreshold consecutive blank rounds; the UID channel is a level
// holding the last read tag (it survives removal by design).
type tagState struct {
	present bool
	uid     string
	misses  int
}

// tagEvent is one debounced change to deliver on a reader channel.
type tagEvent struct {
	ch string // chanUID or chanPresent
	v  engine.Value
}

// apply advances the state machine by one poll result and returns the
// events to deliver, in order: a new UID precedes the present rising
// edge, so logic triggered by the edge already sees the new UID.
func (s *tagState) apply(uid string, found bool) []tagEvent {
	if found {
		var evs []tagEvent
		s.misses = 0
		if uid != s.uid {
			s.uid = uid
			evs = append(evs, tagEvent{chanUID, engine.TextVal(uid)})
		}
		if !s.present {
			s.present = true
			evs = append(evs, tagEvent{chanPresent, engine.BoolVal(true)})
		}
		return evs
	}
	if !s.present {
		return nil
	}
	s.misses++
	if s.misses < missThreshold {
		return nil
	}
	return s.drop()
}

// drop forces the present level down without touching the UID level,
// for tag removal and for a reader that stopped responding.
func (s *tagState) drop() []tagEvent {
	if !s.present {
		s.misses = 0
		return nil
	}
	s.present = false
	s.misses = 0
	return []tagEvent{{chanPresent, engine.BoolVal(false)}}
}

// Monitor owns one persistent poll goroutine per detected reader, from
// startup until shutdown, INDEPENDENT of engine runs. Each poller feeds
// the tag observer (the reader registry) on every tag presentation and,
// while a run is bound to that reader, the run's engine callbacks too
// (-> EnqueueInput, values land at the next tick - the same determinism
// seam as gpio edges and sys polls). One poll goroutine per reader: no
// double poll on a bus. The reader is infrastructure - it reads whether
// or not a graph runs. Runs attach/detach through a RunBinding; they
// never open a device or start a poller.
type Monitor struct {
	mu       sync.Mutex
	readers  map[string]*monReader
	chans    []engine.Channel
	interval time.Duration
	done     chan struct{}
	closed   bool
	wg       sync.WaitGroup
	log      *slog.Logger
}

// monReader is one reader's persistent state. The device is opened at
// Start and polled until Close. owner is the run currently bound to the
// reader's engine channels (nil = only the registry is fed); subs holds
// that owner's per-channel engine callbacks. fresh marks channels bound
// after the poller latched a resting tag, so the next round replays the
// current levels to them once (every bind is "late" now - the poller is
// always live).
//
// Concurrency: owner/subs/fresh are guarded by Monitor.mu. The debounce
// fields (state/errs/warned) are read and written ONLY by this reader's
// single poll goroutine (step), so they need no lock; a second toucher
// would be an unguarded race.
type monReader struct {
	info   ReaderInfo
	dev    tagReader
	owner  *RunBinding
	subs   map[string]func(engine.Value)
	fresh  map[string]bool
	state  tagState
	errs   int
	warned bool
}

// NewMonitor builds the reader monitor from the probed readers, opens
// each device and launches its persistent poll goroutine. It returns
// nil when there are no readers. The caller Close()s it on shutdown; it
// lives for the whole process, feeding the registry regardless of runs.
func NewMonitor(log *slog.Logger) *Monitor {
	mu.Lock()
	defs := append([]detectedReader(nil), detected...)
	mu.Unlock()
	if len(defs) == 0 {
		return nil
	}
	m := newMonitor(defs, log)
	m.start(defs)
	return m
}

// newMonitor builds the monitor struct (channels + reader map) without
// opening any device - the seam tests drive step() through.
func newMonitor(defs []detectedReader, log *slog.Logger) *Monitor {
	if log == nil {
		log = slog.Default()
	}
	m := &Monitor{
		readers:  map[string]*monReader{},
		interval: pollInterval,
		done:     make(chan struct{}),
		log:      log,
	}
	for _, def := range defs {
		m.readers[def.info.ID] = &monReader{
			info: def.info,
			subs: map[string]func(engine.Value){},
		}
		m.chans = append(m.chans,
			engine.Channel{Address: uidAddr(def.info.ID), Label: def.info.Model + " " + def.info.ID + " UID", Kind: engine.Text},
			engine.Channel{Address: presentAddr(def.info.ID), Label: def.info.Model + " " + def.info.ID + " Tag da", Kind: engine.Bool},
		)
	}
	return m
}

// start opens each reader and launches its poller. A reader that fails
// to open is logged and skipped - the others still run and feed the
// registry.
func (m *Monitor) start(defs []detectedReader) {
	for _, def := range defs {
		r := m.readers[def.info.ID]
		dev, err := def.open()
		if err != nil {
			m.log.Warn("nfc reader open failed; skipping", "reader", def.info.ID, "err", err)
			continue
		}
		r.dev = dev
		m.wg.Add(1)
		go m.poll(r)
	}
}

// Channels lists each reader's UID (Text) and present (Bool) channels.
func (m *Monitor) Channels() []engine.Channel { return m.chans }

// RunBinding is a run's engine.Source view over the Monitor: it attaches
// the engine's callbacks to the already-running pollers and detaches
// them on Close. It never opens a device or starts a poller - the reader
// polls continuously regardless.
type RunBinding struct {
	m     *Monitor
	bound map[string]bool // reader ids this binding owns
}

// NewRunBinding returns a binding over the monitor for one engine run.
func (m *Monitor) NewRunBinding() *RunBinding {
	return &RunBinding{m: m, bound: map[string]bool{}}
}

// Channels lists the monitor's reader channels.
func (b *RunBinding) Channels() []engine.Channel { return b.m.chans }

// Subscribe attaches an engine callback to a reader channel. The first
// channel of a reader claims it for this run (loud error when another
// run already owns it); a reader's uid and present channels share the
// binding. The channel's current level is replayed on the next poll
// round (the poller is always live), so a tag resting on the module at
// run start shows on every bound channel within a round.
func (b *RunBinding) Subscribe(addr string, cb func(engine.Value)) error {
	id, ch, ok := splitAddr(addr)
	if !ok || (ch != chanUID && ch != chanPresent) {
		return fmt.Errorf("nfc: unknown channel %q", addr)
	}
	m := b.m
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("nfc: monitor is closed")
	}
	r := m.readers[id]
	if r == nil {
		return fmt.Errorf("nfc: unknown reader %q", id)
	}
	if r.owner != nil && r.owner != b {
		return fmt.Errorf("nfc: reader %s is already in use by another run", id)
	}
	r.owner = b
	b.bound[id] = true
	r.subs[ch] = cb
	if r.fresh == nil {
		r.fresh = map[string]bool{}
	}
	r.fresh[ch] = true
	return nil
}

// Close detaches this run's callbacks and releases the readers it owned;
// the pollers keep running (they feed the registry regardless). A poll
// round that snapshotted this binding's callbacks just before Close may
// still deliver one more value into the run's engine afterwards - that
// is harmless: the run's cleanup runs after its final tick, and
// EnqueueInput on a no-longer-ticking engine only appends to a queue
// that is never drained (no panic, no cross-run leak - the callback
// closes over this run's engine, not the next run's).
func (b *RunBinding) Close() error {
	m := b.m
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range b.bound {
		r := m.readers[id]
		if r != nil && r.owner == b {
			r.owner = nil
			r.subs = map[string]func(engine.Value){}
			r.fresh = nil
		}
	}
	b.bound = map[string]bool{}
	return nil
}

// poll drives one reader until Close. The first poll is immediate so a
// resting tag shows within a frame, not after the first interval.
func (m *Monitor) poll(r *monReader) {
	defer m.wg.Done()
	m.step(r)
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-t.C:
			m.step(r)
		}
	}
}

// step performs one poll round: read the device, advance the debounce
// state machine, feed the registry, and deliver events to the bound
// run's engine callbacks. The subscription map is snapshotted under
// m.mu; the device I/O and the callbacks run OUTSIDE the lock, so cb ->
// EnqueueInput (which takes the engine lock) never nests under m.mu and
// Close never waits on a bus transaction it also holds the lock for. A
// transient poll error keeps the state (an RF hiccup is not a removal);
// errThreshold consecutive errors drop the present level and warn once,
// so a dead reader cannot hold downstream logic active forever.
func (m *Monitor) step(r *monReader) {
	m.mu.Lock()
	subs := make(map[string]func(engine.Value), len(r.subs))
	for ch, cb := range r.subs {
		subs[ch] = cb
	}
	fresh := make(map[string]bool, len(r.fresh))
	for ch := range r.fresh {
		fresh[ch] = true
	}
	dev := r.dev
	m.mu.Unlock()
	if dev == nil {
		return
	}
	uid, found, err := dev.Poll()
	var evs []tagEvent
	// noteUID is set when this round is a tag PRESENTATION for the
	// registry's last-seen tracker: a new/changed UID, or the same tag
	// re-presented (present rising edge, so re-tapping a resting card
	// still refreshes the timestamp). Removal, poll errors and the
	// late-subscriber replay below are not presentations and leave it
	// empty. The actual notify runs after the engine handoff (below).
	noteUID := ""
	if err != nil {
		r.errs++
		if r.errs < errThreshold {
			return // transient; fresh channels stay pending for the next round
		}
		if !r.warned {
			r.warned = true
			m.log.Warn("nfc reader not responding", "reader", r.info.ID, "err", err)
		}
		evs = r.state.drop()
	} else {
		r.errs = 0
		r.warned = false
		s := ""
		if found {
			s = formatUID(uid)
		}
		evs = r.state.apply(s, found)
		if found {
			for _, ev := range evs {
				if ev.ch == chanUID || (ev.ch == chanPresent && ev.v == engine.BoolVal(true)) {
					noteUID = s
				}
			}
		}
	}
	// Late subscribers missed the edges the state machine has already
	// latched; replay the current levels once, unless this round's
	// events cover the channel anyway. The uid level stays first so
	// logic hanging off the present edge sees the right tag.
	if len(fresh) > 0 {
		covered := map[string]bool{}
		for _, ev := range evs {
			covered[ev.ch] = true
		}
		if fresh[chanUID] && !covered[chanUID] {
			evs = append([]tagEvent{{chanUID, engine.TextVal(r.state.uid)}}, evs...)
		}
		if fresh[chanPresent] && !covered[chanPresent] {
			evs = append(evs, tagEvent{chanPresent, engine.BoolVal(r.state.present)})
		}
	}
	for _, ev := range evs {
		if cb := subs[ev.ch]; cb != nil {
			cb(ev.v)
		}
	}
	if len(fresh) > 0 {
		m.mu.Lock()
		for ch := range fresh {
			delete(r.fresh, ch)
		}
		m.mu.Unlock()
	}
	// Registry side-channel LAST: the engine subscribers (the real-time
	// path) are already served, so the observer's work - a SQL write in
	// the reader registry - never delays a tag's delivery to the engine.
	if noteUID != "" {
		notifyTag(r.info.Identity, noteUID)
	}
}

// Close stops all pollers and closes the opened readers. Idempotent;
// called once on process shutdown (the pollers outlive individual runs).
func (m *Monitor) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	close(m.done)
	m.mu.Unlock()
	// Wait for in-flight poll rounds so no goroutine still touches a
	// device we are about to close.
	m.wg.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.readers {
		if r.dev != nil {
			_ = r.dev.Close()
			r.dev = nil
		}
	}
	return nil
}

// Ensure the run binding satisfies the engine Source contract (and is a
// Closer) at compile time. It is deliberately NOT a Sink - tags are
// read-only - and not Configurable (no per-channel options yet).
var (
	_ engine.Source = (*RunBinding)(nil)
	_ io.Closer     = (*RunBinding)(nil)
	_ io.Closer     = (*Monitor)(nil)
)
