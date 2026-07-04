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
// and the detected readers. It is pure and platform-independent so the
// detection logic is unit-testable without hardware: probe returns the
// reader on success, a permission error (errors.Is fs.ErrPermission)
// when the bus cannot be opened, or any other error when there is no
// PN532 on that bus.
func classify(devs []string, probe func(dev string) (detectedReader, error)) (Status, []detectedReader) {
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
			}
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
	// inUse enforces reader exclusivity across runs (see claim).
	inUse  = map[string]bool{}
	logger = slog.Default()
	// probeFn is the platform probe (set in the build-tagged files);
	// tests override it to drive detection without hardware.
	probeFn = platformProbe
)

// Probe detects PN532 readers on the I2C buses once at startup and
// caches the result for Enabled / Readers / NewDriver. It logs the
// outcome - silent when there is simply no reader, a clear actionable
// line on EACCES - and never blocks or panics. Call it once at startup.
func Probe(log *slog.Logger) Status {
	st, readers := probeFn()
	for i := range readers {
		readers[i].info.UIDChannel = engine.PrefixNFC + ":" + uidAddr(readers[i].info.ID)
		readers[i].info.PresentChannel = engine.PrefixNFC + ":" + presentAddr(readers[i].info.ID)
	}
	mu.Lock()
	status = st
	detected = readers
	logger = log
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

// claim reserves a reader for one run. Unlike GPIO lines (the kernel
// enforces line-request exclusivity), /dev/i2c-* has no userspace
// exclusivity - two pollers on one bus would interleave transactions
// and silently corrupt each other's frames - so the package enforces
// it: a second run binding the same reader fails loudly at bind time.
func claim(id string) error {
	mu.Lock()
	defer mu.Unlock()
	if inUse[id] {
		return fmt.Errorf("nfc: reader %s is already in use by another run", id)
	}
	inUse[id] = true
	return nil
}

func release(id string) {
	mu.Lock()
	defer mu.Unlock()
	delete(inUse, id)
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

// Driver is a live NFC source driver: one poll goroutine per bound
// reader feeds debounced tag events into the engine via the subscribe
// callbacks (-> EnqueueInput, values land at the next tick - the same
// determinism seam as gpio edges and sys polls). One instance per run;
// Close stops the pollers and releases the readers.
type Driver struct {
	mu       sync.Mutex
	readers  map[string]*runReader
	chans    []engine.Channel
	interval time.Duration
	done     chan struct{}
	closed   bool
	wg       sync.WaitGroup
	log      *slog.Logger
}

// runReader is one reader's run-scoped state. The device is opened (and
// the reader claimed) lazily on the first Subscribe that touches it, so
// a run binding only reader A never claims reader B. The debounce state
// and error counters are owned by the reader's poll goroutine. fresh
// marks channels subscribed AFTER the poller started: the state machine
// may already have latched their edges (a tag resting on the module
// since before the bind), so the next poll round replays the current
// levels to them once.
type runReader struct {
	info    ReaderInfo
	open    func() (tagReader, error)
	dev     tagReader
	claimed bool
	started bool
	subs    map[string]func(engine.Value)
	fresh   map[string]bool
	state   tagState
	errs    int
	warned  bool
}

// NewDriver builds a fresh NFC driver from the probed readers, for one
// run. It opens nothing yet - devices are claimed and opened on the
// first Subscribe. The caller registers it under engine.PrefixNFC and
// Close()s it on teardown. Errors on a host with no readers (where
// Enabled() is false and this is never reached).
func NewDriver() (*Driver, error) {
	mu.Lock()
	defs := append([]detectedReader(nil), detected...)
	log := logger
	mu.Unlock()
	if len(defs) == 0 {
		return nil, errors.New("nfc: no readers available")
	}
	d := &Driver{
		readers:  map[string]*runReader{},
		interval: pollInterval,
		done:     make(chan struct{}),
		log:      log,
	}
	for _, def := range defs {
		d.readers[def.info.ID] = &runReader{
			info: def.info,
			open: def.open,
			subs: map[string]func(engine.Value){},
		}
		d.chans = append(d.chans,
			engine.Channel{Address: uidAddr(def.info.ID), Label: def.info.Model + " " + def.info.ID + " UID", Kind: engine.Text},
			engine.Channel{Address: presentAddr(def.info.ID), Label: def.info.Model + " " + def.info.ID + " Tag da", Kind: engine.Bool},
		)
	}
	return d, nil
}

// Channels lists each reader's UID (Text) and present (Bool) channels.
func (d *Driver) Channels() []engine.Channel { return d.chans }

// Subscribe binds an engine callback to a reader channel. The first
// subscription touching a reader claims it (loud error when another run
// holds it), opens and configures the device, and starts its poll
// goroutine; a reader's uid and present channels share both. The
// channels start at their kind's zero values (no tag); the first poll
// round runs immediately, and a channel subscribed after the poller
// already latched a resting tag gets the current levels replayed on the
// next round - so a tag on the module at run start shows on every bound
// channel within a poll round.
func (d *Driver) Subscribe(addr string, cb func(engine.Value)) error {
	id, ch, ok := splitAddr(addr)
	if !ok || (ch != chanUID && ch != chanPresent) {
		return fmt.Errorf("nfc: unknown channel %q", addr)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return errors.New("nfc: driver is closed")
	}
	r := d.readers[id]
	if r == nil {
		return fmt.Errorf("nfc: unknown reader %q", id)
	}
	r.subs[ch] = cb
	if r.started {
		// The poll goroutine is live: any edge for this channel may
		// already be latched in the state machine and would never
		// re-fire. Have the next round replay the current levels.
		if r.fresh == nil {
			r.fresh = map[string]bool{}
		}
		r.fresh[ch] = true
		return nil
	}
	if err := claim(id); err != nil {
		return err
	}
	dev, err := r.open()
	if err != nil {
		release(id)
		return fmt.Errorf("open reader %s: %w", id, err)
	}
	r.claimed = true
	r.dev = dev
	r.started = true
	d.wg.Add(1)
	go d.poll(r)
	return nil
}

// poll drives one reader until Close. The first poll is immediate so a
// resting tag shows within a frame, not after the first interval.
func (d *Driver) poll(r *runReader) {
	defer d.wg.Done()
	d.step(r)
	t := time.NewTicker(d.interval)
	defer t.Stop()
	for {
		select {
		case <-d.done:
			return
		case <-t.C:
			d.step(r)
		}
	}
}

// step performs one poll round: read the device, advance the debounce
// state machine, deliver the resulting events. The subscription map is
// snapshotted under d.mu; the device I/O and the callbacks run OUTSIDE
// the lock, so cb -> EnqueueInput (which takes the engine lock) never
// nests under d.mu and Close never waits on a bus transaction it also
// holds the lock for. A transient poll error keeps the state (an RF
// hiccup is not a removal); errThreshold consecutive errors drop the
// present level and warn once, so a dead reader cannot hold downstream
// logic active forever.
func (d *Driver) step(r *runReader) {
	d.mu.Lock()
	subs := make(map[string]func(engine.Value), len(r.subs))
	for ch, cb := range r.subs {
		subs[ch] = cb
	}
	fresh := make(map[string]bool, len(r.fresh))
	for ch := range r.fresh {
		fresh[ch] = true
	}
	dev := r.dev
	d.mu.Unlock()
	if dev == nil {
		return
	}
	uid, found, err := dev.Poll()
	var evs []tagEvent
	if err != nil {
		r.errs++
		if r.errs < errThreshold {
			return // transient; fresh channels stay pending for the next round
		}
		if !r.warned {
			r.warned = true
			d.log.Warn("nfc reader not responding", "reader", r.info.ID, "err", err)
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
		d.mu.Lock()
		for ch := range fresh {
			delete(r.fresh, ch)
		}
		d.mu.Unlock()
	}
}

// Close stops all pollers, closes the opened readers and releases their
// claims. Idempotent. The run layer calls it once, after the final tick,
// so it never overlaps a callback delivery into a live engine.
func (d *Driver) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	close(d.done)
	d.mu.Unlock()
	// Wait for in-flight poll rounds so no goroutine still touches a
	// device we are about to close.
	d.wg.Wait()
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, r := range d.readers {
		if r.dev != nil {
			_ = r.dev.Close()
			r.dev = nil
		}
		if r.claimed {
			release(r.info.ID)
			r.claimed = false
		}
	}
	return nil
}

// Ensure the driver satisfies the engine Source contract (and is a
// Closer) at compile time. It is deliberately NOT a Sink - tags are
// read-only in this ticket - and not Configurable (no per-channel
// options yet).
var (
	_ engine.Source = (*Driver)(nil)
	_ io.Closer     = (*Driver)(nil)
)
