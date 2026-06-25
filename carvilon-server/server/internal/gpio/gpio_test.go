package gpio

import (
	"io"
	"io/fs"
	"log/slog"
	"testing"
)

type fakeCloser struct{ closed *bool }

func (f fakeCloser) Close() error {
	if f.closed != nil {
		*f.closed = true
	}
	return nil
}

// TestClassify covers the three detection outcomes without any real
// hardware, by feeding classify a fake device list and opener.
func TestClassify(t *testing.T) {
	okOpen := func(string) (io.Closer, error) { return fakeCloser{}, nil }
	eaccesOpen := func(string) (io.Closer, error) {
		return nil, &fs.PathError{Op: "open", Path: "/dev/gpiochip0", Err: fs.ErrPermission}
	}
	otherOpen := func(string) (io.Closer, error) { return nil, io.ErrUnexpectedEOF }

	if st, chips := classify(nil, okOpen); st != Unavailable || chips != nil {
		t.Errorf("no devices: got (%v, %v), want (Unavailable, nil)", st, chips)
	}
	if st, chips := classify([]string{"/dev/gpiochip0", "/dev/gpiochip1"}, okOpen); st != Available || len(chips) != 2 || chips[0] != "gpiochip0" {
		t.Errorf("openable: got (%v, %v), want Available with chip basenames", st, chips)
	}
	if st, _ := classify([]string{"/dev/gpiochip0"}, eaccesOpen); st != Forbidden {
		t.Errorf("EACCES: got %v, want Forbidden", st)
	}
	if st, _ := classify([]string{"/dev/gpiochip0"}, otherOpen); st != Unavailable {
		t.Errorf("non-permission error: got %v, want Unavailable", st)
	}
}

// TestClassifyClosesChips guards that classify releases the chips it
// probed (no leaked open handles from detection).
func TestClassifyClosesChips(t *testing.T) {
	var closed bool
	open := func(string) (io.Closer, error) { return fakeCloser{closed: &closed}, nil }
	if st, _ := classify([]string{"/dev/gpiochip0"}, open); st != Available || !closed {
		t.Errorf("classify must close the probed chip; status=%v closed=%v", st, closed)
	}
}

// TestParseLine covers the chip:offset address parsing (pure, so tested
// on any platform even though the driver that uses it is Linux-only).
func TestParseLine(t *testing.T) {
	chip, off, err := parseLine("gpiochip0:17")
	if err != nil || chip != "gpiochip0" || off != 17 {
		t.Errorf("parseLine(gpiochip0:17) = (%q,%d,%v), want (gpiochip0,17,nil)", chip, off, err)
	}
	for _, bad := range []string{"", "gpiochip0", "nocolon", "gpiochip0:", "gpiochip0:-1", "gpiochip0:abc", ":17"} {
		if _, _, err := parseLine(bad); err == nil {
			t.Errorf("parseLine(%q) should error", bad)
		}
	}
}

// TestLineAddress pins the binding-address form the picker writes and the
// run-strecke parses.
func TestLineAddress(t *testing.T) {
	if got := lineAddress("gpiochip0", 17); got != "gpio:gpiochip0:17" {
		t.Errorf("lineAddress = %q, want gpio:gpiochip0:17", got)
	}
}

// TestLinesEmptyWithoutGPIO: on a host with no GPIO the line list is empty
// (nothing GPIO surfaces in the picker).
func TestLinesEmptyWithoutGPIO(t *testing.T) {
	if lines := Lines(); len(lines) != 0 {
		t.Errorf("Lines() without GPIO = %v, want empty", lines)
	}
}

// TestUsableLine pins the picker's usable-vs-system heuristic: a generic
// GPIOnn name (or no name) that is free is usable; a dedicated-function
// name or an in-use line is not. Not board-specific - name + in-use only.
func TestUsableLine(t *testing.T) {
	cases := []struct {
		name   string
		inUse  bool
		usable bool
	}{
		{"GPIO17", false, true},      // generic header GPIO
		{"gpio3", false, true},       // case-insensitive
		{"", false, true},            // unnamed line - treat as usable
		{"GPIO17", true, false},      // generic but occupied
		{"RGMII_MDIO", false, false}, // Ethernet
		{"SD1_CLK", false, false},    // SD card
		{"WL_ON", false, false},      // WLAN enable
		{"STATUS_LED", false, false},
		{"PWM0", false, false},
		{"GPIO", false, false},  // "GPIO" with no number is not generic
		{"GPIOA", false, false}, // letters after GPIO are not generic
	}
	for _, c := range cases {
		if got := usableLine(c.name, c.inUse); got != c.usable {
			t.Errorf("usableLine(%q, inUse=%v) = %v, want %v", c.name, c.inUse, got, c.usable)
		}
	}
}

// TestProbeAvailability checks Probe caches availability per outcome.
// Forbidden must NOT count as available (chip present but no access).
func TestProbeAvailability(t *testing.T) {
	old := probeFn
	defer func() { probeFn = old }()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	cases := []struct {
		name string
		st   Status
		want bool
	}{
		{"unavailable", Unavailable, false},
		{"forbidden", Forbidden, false},
		{"available", Available, true},
	}
	for _, c := range cases {
		probeFn = func() (Status, []string) {
			if c.st == Available {
				return Available, []string{"gpiochip0"}
			}
			return c.st, nil
		}
		if got := Probe(log); got != c.st {
			t.Errorf("%s: Probe returned %v, want %v", c.name, got, c.st)
		}
		if Enabled() != c.want {
			t.Errorf("%s: Enabled() = %v, want %v", c.name, Enabled(), c.want)
		}
	}
}

// TestProbeDefaultNoHardware: on the dev machine (no probeFn override)
// the platform probe must report no GPIO, so nothing GPIO surfaces.
func TestProbeDefaultNoHardware(t *testing.T) {
	old := probeFn
	defer func() { probeFn = old }()
	probeFn = platformProbe // the real per-platform probe
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// On non-Linux this is Unavailable by construction; on a Linux CI box
	// without accessible chips it is Unavailable/Forbidden - never panics,
	// and Enabled() is false unless a real chip is openable.
	st := Probe(log)
	if st == Available && !Enabled() {
		t.Errorf("inconsistent: Probe said Available but Enabled()=false")
	}
}
