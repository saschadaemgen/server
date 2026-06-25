//go:build linux

package gpio

import (
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"

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

// Driver is the live GPIO driver: input lines are requested with edge
// detection and feed the engine's async queue via the bound callback;
// output lines are driven on change. All lines are Bool (digital pins -
// the T1 Bool boundary). Goroutine-safe; one instance per run, Close()d
// on teardown to release every line.
type Driver struct {
	mu    sync.Mutex
	lines map[string]*gpiocdev.Line
	chans []engine.Channel
	log   *slog.Logger
}

func newDriver(chips []string) (IODriver, error) {
	d := &Driver{lines: map[string]*gpiocdev.Line{}, log: currentLogger()}
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

// Subscribe requests an input line with both-edge detection. The edge
// handler (a go-gpiocdev goroutine) reports the new logical level through
// cb, which routes to Engine.EnqueueInput - the value lands at the next
// tick, never concurrently with eval. The handler is intentionally tiny.
//
// The line is pull-up + active-LOW, matching the common button-to-ground
// wiring: idle reads false (so a run does not boot "on") and pressed reads
// true (a rising edge that triggers downstream logic). A different wiring
// is a later, per-line config option.
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
	l, err := gpiocdev.RequestLine(chip, off,
		gpiocdev.AsInput,
		gpiocdev.AsActiveLow,
		gpiocdev.WithPullUp,
		gpiocdev.WithBothEdges,
		gpiocdev.WithEventHandler(func(ev gpiocdev.LineEvent) {
			cb(engine.BoolVal(ev.Type == gpiocdev.LineEventRisingEdge))
		}),
	)
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

// Write drives an output line, requesting it as output (with the initial
// value) on first use and SetValue-ing it thereafter. Called from inside
// a tick under the engine lock, so it never blocks or re-enters the
// engine.
func (d *Driver) Write(addr string, v engine.Value) error {
	chip, off, err := parseLine(addr)
	if err != nil {
		return err
	}
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
	l, err := gpiocdev.RequestLine(chip, off, gpiocdev.AsOutput(val))
	if err != nil {
		d.log.Warn("gpio output request failed", "line", addr, "err", err)
		return fmt.Errorf("gpio: request output %s: %w", addr, err)
	}
	d.lines[addr] = l
	return nil
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
