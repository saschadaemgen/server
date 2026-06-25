//go:build linux

package gpio

import (
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/warthog618/go-gpiocdev"

	"carvilon.local/server/internal/engine"
)

// platformProbe globs the GPIO character devices and tries to open each,
// so it can tell "no chip" from "chip present but EACCES" - the glob only
// needs to read /dev, the open is what hits permissions.
func platformProbe() (Status, []string) {
	devs, _ := filepath.Glob("/dev/gpiochip*")
	return classify(devs, func(name string) (io.Closer, error) {
		c, err := gpiocdev.NewChip(name)
		if err != nil {
			return nil, err
		}
		return c, nil
	})
}

// platformLines reads each detected chip's lines (offset, kernel name,
// in-use flag) read-only via go-gpiocdev, for the editor's pin picker.
func platformLines(chips []string) []LineInfo {
	var out []LineInfo
	for _, name := range chips {
		c, err := gpiocdev.NewChip(name)
		if err != nil {
			continue
		}
		n := c.Lines()
		for off := 0; off < n; off++ {
			li := LineInfo{Address: lineAddress(name, off), Chip: name, Offset: off}
			if info, err := c.LineInfo(off); err == nil {
				li.Name = info.Name
				li.InUse = info.Used
			} else {
				// Fail closed: a line we cannot inspect is treated as
				// in-use so the picker renders it unavailable rather than
				// offering a possibly system-held line.
				li.InUse = true
			}
			li.Usable = usableLine(li.Name, li.InUse)
			out = append(out, li)
		}
		_ = c.Close()
	}
	return out
}

// Driver is the live GPIO driver: input lines are requested with edge
// detection and feed the engine's async queue via the bound callback;
// output lines are driven on change. All lines are Bool (digital pins -
// the T1 Bool boundary). Goroutine-safe; one instance per run, Close()d
// on teardown to release every line.
type Driver struct {
	mu    sync.Mutex
	lines map[string]*gpiocdev.Line
	cfgs  map[string]engine.ChannelConfig // per-line options from BindGraph
	chans []engine.Channel
	log   *slog.Logger
}

func newDriver(chips []string) (IODriver, error) {
	d := &Driver{
		lines: map[string]*gpiocdev.Line{},
		cfgs:  map[string]engine.ChannelConfig{},
		log:   currentLogger(),
	}
	for _, name := range chips {
		c, err := gpiocdev.NewChip(name)
		if err != nil {
			_ = d.Close()
			return nil, fmt.Errorf("gpio: open %s: %w", name, err)
		}
		n := c.Lines()
		_ = c.Close()
		for off := 0; off < n; off++ {
			d.chans = append(d.chans, engine.Channel{
				Address: fmt.Sprintf("%s:%d", name, off),
				Label:   fmt.Sprintf("%s line %d", name, off),
				Kind:    engine.Bool,
			})
		}
	}
	return d, nil
}

// Channels lists every line of every detected chip as a Bool channel.
func (d *Driver) Channels() []engine.Channel { return d.chans }

// ConfigureInput records an input line's options (bias / active level /
// debounce) so the subsequent Subscribe requests the line with them. It
// only stores; Subscribe does the request (it owns the edge callback).
func (d *Driver) ConfigureInput(addr string, cfg engine.ChannelConfig) error {
	d.mu.Lock()
	d.cfgs[addr] = cfg
	d.mu.Unlock()
	return nil
}

// ConfigureOutput records an output line's options and PRE-ACQUIRES the
// line at its configured initial state, so the pin holds that level from
// run start until the engine first drives it (the engine writes only on
// transitions, so without this an unchanged output would never be
// requested). Idempotent if the line is already held.
func (d *Driver) ConfigureOutput(addr string, cfg engine.ChannelConfig) error {
	chip, off, err := parseLine(addr)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cfgs[addr] = cfg
	if _, ok := d.lines[addr]; ok {
		return nil
	}
	l, err := gpiocdev.RequestLine(chip, off, outputOptions(cfg, initialValue(cfg))...)
	if err != nil {
		d.log.Warn("gpio output request failed", "line", addr, "err", err)
		return fmt.Errorf("gpio: request output %s: %w", addr, err)
	}
	d.lines[addr] = l
	return nil
}

// Subscribe requests an input line with both-edge detection. The edge
// handler (a go-gpiocdev goroutine) reports the new logical level through
// cb, which routes to Engine.EnqueueInput - the value lands at the next
// tick, never concurrently with eval. The handler is intentionally tiny.
//
// Bias / active level / debounce come from the line's config (set by
// ConfigureInput); with nothing set the defaults are pull-up + active-LOW,
// the common button-to-ground wiring where idle reads false (a run does
// not boot "on") and pressed reads true. Edges are reported against the
// active level, so rising == logical-active regardless of polarity.
func (d *Driver) Subscribe(addr string, cb func(engine.Value)) error {
	chip, off, err := parseLine(addr)
	if err != nil {
		return err
	}
	d.mu.Lock()
	if _, dup := d.lines[addr]; dup {
		d.mu.Unlock()
		return fmt.Errorf("gpio: line %s already requested", addr)
	}
	opts := append(inputOptions(d.cfgs[addr]),
		gpiocdev.WithEventHandler(func(ev gpiocdev.LineEvent) {
			cb(engine.BoolVal(ev.Type == gpiocdev.LineEventRisingEdge))
		}),
	)
	l, err := gpiocdev.RequestLine(chip, off, opts...)
	if err != nil {
		d.mu.Unlock()
		return fmt.Errorf("gpio: request input %s: %w", addr, err)
	}
	d.lines[addr] = l
	d.mu.Unlock()
	// Deliver the current level so the source node starts at the real pin
	// state (Source contract). Done after releasing d.mu so cb -> e.mu is
	// never taken under d.mu (the sink path takes e.mu then d.mu).
	if v, err := l.Value(); err == nil {
		cb(engine.BoolVal(v != 0))
	}
	return nil
}

// Write drives an output line, requesting it (with its options + this
// value as the initial level) on first use if ConfigureOutput has not
// already acquired it, and SetValue-ing it thereafter. Called from inside
// a tick under the engine lock, so it never blocks or re-enters the engine.
func (d *Driver) Write(addr string, v engine.Value) error {
	val := 0
	if v.B {
		val = 1
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if l, ok := d.lines[addr]; ok {
		if err := l.SetValue(val); err != nil {
			d.log.Warn("gpio output write failed", "line", addr, "err", err)
			return err
		}
		return nil
	}
	chip, off, err := parseLine(addr)
	if err != nil {
		return err
	}
	l, err := gpiocdev.RequestLine(chip, off, outputOptions(d.cfgs[addr], val)...)
	if err != nil {
		d.log.Warn("gpio output request failed", "line", addr, "err", err)
		return fmt.Errorf("gpio: request output %s: %w", addr, err)
	}
	d.lines[addr] = l
	return nil
}

// inputOptions builds the go-gpiocdev request options for an input line
// from its config, defaulting to pull-up + active-low (the established
// button-to-ground default) when a key is unset.
func inputOptions(cfg engine.ChannelConfig) []gpiocdev.LineReqOption {
	opts := []gpiocdev.LineReqOption{gpiocdev.AsInput, gpiocdev.WithBothEdges}
	switch cfg["bias"] {
	case "pulldown":
		opts = append(opts, gpiocdev.WithPullDown)
	case "none":
		opts = append(opts, gpiocdev.WithBiasDisabled)
	default: // "pullup" or unset
		opts = append(opts, gpiocdev.WithPullUp)
	}
	if cfg["active_level"] == "high" {
		opts = append(opts, gpiocdev.AsActiveHigh)
	} else { // "low" or unset
		opts = append(opts, gpiocdev.AsActiveLow)
	}
	if ms, err := strconv.Atoi(cfg["debounce_ms"]); err == nil && ms > 0 {
		opts = append(opts, gpiocdev.WithDebounce(time.Duration(ms)*time.Millisecond))
	}
	return opts
}

// outputOptions builds the request options for an output line: the given
// initial LOGICAL value plus the active level (default active-high, the
// established output default).
func outputOptions(cfg engine.ChannelConfig, initial int) []gpiocdev.LineReqOption {
	opts := []gpiocdev.LineReqOption{gpiocdev.AsOutput(initial)}
	if cfg["active_level"] == "low" {
		opts = append(opts, gpiocdev.AsActiveLow)
	} else { // "high" or unset
		opts = append(opts, gpiocdev.AsActiveHigh)
	}
	return opts
}

// initialValue is the configured initial output level (default low).
func initialValue(cfg engine.ChannelConfig) int {
	if cfg["initial"] == "high" {
		return 1
	}
	return 0
}

// Close releases every requested line (stopping the edge watchers) and
// resets the driver. Idempotent.
func (d *Driver) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, l := range d.lines {
		_ = l.Close()
	}
	d.lines = map[string]*gpiocdev.Line{}
	return nil
}
