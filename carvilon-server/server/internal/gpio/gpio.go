// Package gpio is a runtime-detected GPIO driver for the engine adapter
// layer (it implements engine.Source/Sink under the "gpio:" prefix). It
// is a pure addition on top of the T1 seam - the engine itself is
// unchanged. The real driver (Linux, via go-gpiocdev over the GPIO
// character device) lives in the build-tagged files; on every other
// platform it degrades cleanly to "no GPIO". Detection is at runtime, not
// build time: a host either has accessible gpiochips or it does not.
package gpio

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"carvilon.local/server/internal/engine"
)

// Status is the outcome of probing the host for usable GPIO.
type Status int

const (
	// Unavailable: no gpiochip device on this host (VPS / mini-PC).
	Unavailable Status = iota
	// Forbidden: a gpiochip exists but the process cannot open it
	// (EACCES) - the service user needs to be in the gpio group.
	Forbidden
	// Available: at least one gpiochip is present and openable.
	Available
)

// IODriver is a live GPIO driver instance: both a Source and a Sink over
// the engine adapter layer, Configurable (it applies per-line bias /
// active level / debounce / initial-state options at bind time), plus a
// Closer that releases every requested line on teardown.
type IODriver interface {
	engine.Source
	engine.Sink
	engine.Configurable
	io.Closer
}

// LineInfo describes one GPIO line for the editor's pin picker: the
// binding address the graph stores, the chip + offset, the kernel line
// name (may be empty), whether the line is already consumed by the
// system / another process (so the picker can exclude it), and whether it
// is a usable general-purpose GPIO (vs a system/peripheral line the
// picker hides by default).
type LineInfo struct {
	Address string `json:"address"` // "gpio:gpiochipN:offset"
	Chip    string `json:"chip"`
	Offset  int    `json:"offset"`
	Name    string `json:"name"`
	InUse   bool   `json:"inUse"`
	Usable  bool   `json:"usable"`
}

// usableLine reports whether a line is a general-purpose GPIO suitable for
// the picker's default view, as opposed to a system/peripheral line. The
// signal is the kernel line name plus the in-use flag - NOT a hardcoded
// board map: a line in use by the system/another process, or one with a
// dedicated-function name (RGMII_*, SD_*, WL_ON, STATUS_LED, ...), is not
// a free general GPIO; a generic "GPIOnn" name, or no name at all, is.
func usableLine(name string, inUse bool) bool {
	if inUse {
		return false
	}
	return name == "" || genericGPIOName(name)
}

// genericGPIOName matches the kernel's generic GPIO line naming ("GPIO17",
// "gpio3") - "GPIO" followed by digits - as opposed to a dedicated
// peripheral function name.
func genericGPIOName(name string) bool {
	if len(name) < 5 { // "GPIO" + at least one digit
		return false
	}
	if !strings.EqualFold(name[:4], "GPIO") {
		return false
	}
	for _, r := range name[4:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// lineAddress builds the binding address for a chip line. It is the
// single place the "gpio:chip:offset" form is produced, and matches what
// the run-strecke parses (ParsePhysical + the driver's parseLine).
func lineAddress(chip string, offset int) string {
	return "gpio:" + chip + ":" + strconv.Itoa(offset)
}

// Lines returns the detected GPIO lines (offset, kernel name, in-use) for
// the editor's pin picker, read read-only via go-gpiocdev. Empty on a
// non-GPIO host. It is read LIVE per call, not cached: the in-use bit is
// runtime-mutable (a line a run currently holds reads as used), so a
// frozen snapshot would mislead the picker. The endpoint is admin-only
// and infrequent, so the per-call chip enumeration is cheap enough.
func Lines() []LineInfo {
	return platformLines(detectedChips())
}

// classify turns candidate device paths plus an opener into a Status and
// the openable chip names. It is pure and platform-independent so the
// detection logic is unit-testable without real hardware: open returns a
// closer on success, or a permission error (errors.Is fs.ErrPermission)
// for EACCES, or any other error for a device that is not a usable chip.
func classify(devs []string, open func(name string) (io.Closer, error)) (Status, []string) {
	if len(devs) == 0 {
		return Unavailable, nil
	}
	var chips []string
	forbidden := false
	for _, dev := range devs {
		name := filepath.Base(dev)
		c, err := open(name)
		if err != nil {
			if errors.Is(err, fs.ErrPermission) {
				forbidden = true
			}
			continue
		}
		_ = c.Close()
		chips = append(chips, name)
	}
	if len(chips) > 0 {
		return Available, chips
	}
	if forbidden {
		return Forbidden, nil
	}
	return Unavailable, nil
}

var (
	mu        sync.Mutex
	available bool
	chipNames []string
	logger    = slog.Default() // replaced by the server logger in Probe
	// probeFn is the platform probe (set in linux.go / other.go); tests
	// override it to drive the three detection outcomes.
	probeFn = platformProbe
)

// Probe detects accessible gpiochips once at startup and caches the
// result for Available / NewDriver. It logs the outcome - silent when
// there is simply no GPIO, a clear actionable line on EACCES - and never
// blocks or panics. Call it once during startup.
func Probe(log *slog.Logger) Status {
	st, chips := probeFn()
	mu.Lock()
	available = st == Available
	chipNames = chips
	logger = log
	mu.Unlock()
	switch st {
	case Available:
		log.Info("gpio available", "chips", chips)
	case Forbidden:
		log.Warn("gpio detected but access denied (EACCES); add the service user to the gpio group " +
			"(e.g. `sudo usermod -aG gpio <user>`) and restart - GPIO disabled for now")
	case Unavailable:
		// No gpiochip on this host (VPS/mini-PC): stay silent; nothing
		// GPIO surfaces in the catalog or runs.
	}
	return st
}

// Enabled reports whether Probe found usable GPIO. The catalog and the
// run path key the GPIO surface off this.
func Enabled() bool {
	mu.Lock()
	defer mu.Unlock()
	return available
}

// detectedChips returns a copy of the chip names Probe found openable.
func detectedChips() []string {
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), chipNames...)
}

// NewDriver builds a fresh GPIO driver instance (opening the detected
// chips) for one run. The caller registers it under engine.PrefixGPIO and
// Close()s it on teardown to release the lines. It errors on a non-GPIO
// host (where Enabled() is false and this is never reached).
func NewDriver() (IODriver, error) { return newDriver(detectedChips()) }

// currentLogger returns the logger Probe cached (the server logger after
// startup); the driver captures it to report pin-op failures.
func currentLogger() *slog.Logger {
	mu.Lock()
	defer mu.Unlock()
	return logger
}

// parseLine splits "gpiochip0:17" into the chip name and line offset. It
// lives in this untagged file so it is unit-tested on any platform.
func parseLine(addr string) (string, int, error) {
	chip, offs, ok := strings.Cut(addr, ":")
	if !ok || chip == "" {
		return "", 0, fmt.Errorf("gpio: bad line address %q (want chip:offset)", addr)
	}
	off, err := strconv.Atoi(offs)
	if err != nil || off < 0 {
		return "", 0, fmt.Errorf("gpio: bad line offset in %q", addr)
	}
	return chip, off, nil
}
