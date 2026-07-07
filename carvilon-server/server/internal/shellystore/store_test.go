package shellystore

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"carvilon.local/server/internal/db"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newTestStore opens a real temp-file DB so the full migration stack
// (including migration 038, shelly_devices) runs.
func newTestStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	c := &clock{t: time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)}
	return New(d.DB, WithClock(c.now)), c
}

const capN = 32

func addrs(ds []Device) map[string]Device {
	m := make(map[string]Device, len(ds))
	for _, d := range ds {
		m[d.Address] = d
	}
	return m
}

// TestAutoAdopt: a fresh discovered device joins the active set once.
func TestAutoAdopt(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	res, err := s.Adopt(ctx, Detected{MAC: "08F9E0E5C790", Address: "192.168.1.51", Name: "Growbox", Model: "Shelly Pro4PM"}, capN)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if res != AdoptedNew {
		t.Fatalf("first adopt = %v, want AdoptedNew", res)
	}
	// Second announcement of the same device: known, not a duplicate.
	res, err = s.Adopt(ctx, Detected{MAC: "08F9E0E5C790", Address: "192.168.1.51", Name: "Growbox"}, capN)
	if err != nil {
		t.Fatalf("adopt 2: %v", err)
	}
	if res != AdoptedKnown {
		t.Fatalf("second adopt = %v, want AdoptedKnown", res)
	}
	active, err := s.ListActive(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("active = %d, want 1", len(active))
	}
	if active[0].Origin != OriginDiscovered {
		t.Fatalf("origin = %q, want discovered", active[0].Origin)
	}
}

// TestStickyRemoval is the core test: a removed device stays gone across a
// re-announcement, and is released cleanly.
func TestStickyRemoval(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	dev := Detected{MAC: "08F9E0E5C790", Address: "192.168.1.51", Name: "Growbox"}

	if _, err := s.Adopt(ctx, dev, capN); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if err := s.RemoveByAddress(ctx, "192.168.1.51"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	// It must be gone from the active set...
	if active, _ := s.ListActive(ctx); len(active) != 0 {
		t.Fatalf("active after remove = %d, want 0", len(active))
	}
	// ...and it must be on the ignore list.
	ign, _ := s.ListIgnored(ctx)
	if len(ign) != 1 {
		t.Fatalf("ignored = %d, want 1", len(ign))
	}

	// STICKY: the device keeps announcing - it must NOT come back.
	res, err := s.Adopt(ctx, dev, capN)
	if err != nil {
		t.Fatalf("re-adopt: %v", err)
	}
	if res != AdoptSkippedIgnored {
		t.Fatalf("re-adopt = %v, want AdoptSkippedIgnored", res)
	}
	if active, _ := s.ListActive(ctx); len(active) != 0 {
		t.Fatalf("active after re-announce = %d, want 0 (STICKY FAILED)", len(active))
	}

	// Sticky even when the device's DHCP address changed (MAC is durable).
	res, _ = s.Adopt(ctx, Detected{MAC: "08F9E0E5C790", Address: "192.168.1.99"}, capN)
	if res != AdoptSkippedIgnored {
		t.Fatalf("re-adopt at new address = %v, want AdoptSkippedIgnored (MAC stickiness FAILED)", res)
	}

	// Release: the ignore row goes away, and the next announcement adopts.
	if len(ign) == 0 {
		t.Fatal("no ignored row to release")
	}
	if err := s.ReleaseByID(ctx, ign[0].ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	res, err = s.Adopt(ctx, dev, capN)
	if err != nil {
		t.Fatalf("adopt after release: %v", err)
	}
	if res != AdoptedNew {
		t.Fatalf("adopt after release = %v, want AdoptedNew", res)
	}
	if active, _ := s.ListActive(ctx); len(active) != 1 {
		t.Fatalf("active after release+announce = %d, want 1", len(active))
	}
}

// TestStickyRemovalByAddressOnly: a manual IP that was never reached (no
// MAC) is removed by address and stays gone.
func TestStickyRemovalByAddressOnly(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if err := s.ReplaceManual(ctx, []string{"192.168.1.60"}); err != nil {
		t.Fatalf("replace manual: %v", err)
	}
	if err := s.RemoveByAddress(ctx, "192.168.1.60"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	// An announcement arriving at that address (now with a MAC) is still
	// ignored, because the ignore match is on the exact address.
	res, err := s.Adopt(ctx, Detected{MAC: "AABBCCDDEEFF", Address: "192.168.1.60"}, capN)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if res != AdoptSkippedIgnored {
		t.Fatalf("adopt at ignored address = %v, want AdoptSkippedIgnored", res)
	}
}

// TestManualUpgradedByDiscovery: a manual IP row gets its MAC filled in
// when the same device is discovered - no duplicate row appears.
func TestManualUpgradedByDiscovery(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if err := s.ReplaceManual(ctx, []string{"192.168.1.51"}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	res, err := s.Adopt(ctx, Detected{MAC: "08F9E0E5C790", Address: "192.168.1.51", Name: "Growbox"}, capN)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if res != AdoptedKnown {
		t.Fatalf("adopt of manual addr = %v, want AdoptedKnown", res)
	}
	active, _ := s.ListActive(ctx)
	if len(active) != 1 {
		t.Fatalf("active = %d, want 1 (duplicate created?)", len(active))
	}
	if active[0].MAC != "08F9E0E5C790" {
		t.Fatalf("mac = %q, want the discovered MAC", active[0].MAC)
	}
	if active[0].Origin != OriginManual {
		t.Fatalf("origin = %q, want manual (kept)", active[0].Origin)
	}
}

// TestReplaceManualReconciles: adds and removes track the settings list,
// while discovered and ignored rows are untouched.
func TestReplaceManualReconciles(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// A discovered device and a sticky-ignored device exist independently.
	if _, err := s.Adopt(ctx, Detected{MAC: "111111111111", Address: "192.168.1.10"}, capN); err != nil {
		t.Fatalf("adopt disc: %v", err)
	}
	if _, err := s.Adopt(ctx, Detected{MAC: "222222222222", Address: "192.168.1.20"}, capN); err != nil {
		t.Fatalf("adopt to-ignore: %v", err)
	}
	if err := s.RemoveByAddress(ctx, "192.168.1.20"); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if err := s.ReplaceManual(ctx, []string{"192.168.1.51", "192.168.1.52"}); err != nil {
		t.Fatalf("replace 1: %v", err)
	}
	// Drop .52, add .53.
	if err := s.ReplaceManual(ctx, []string{"192.168.1.51", "192.168.1.53"}); err != nil {
		t.Fatalf("replace 2: %v", err)
	}
	active, _ := s.ListActive(ctx)
	m := addrs(active)
	for _, want := range []string{"192.168.1.10", "192.168.1.51", "192.168.1.53"} {
		if _, ok := m[want]; !ok {
			t.Fatalf("missing active %s; got %v", want, keys(m))
		}
	}
	if _, ok := m["192.168.1.52"]; ok {
		t.Fatal(".52 should have been removed")
	}
	if _, ok := m["192.168.1.20"]; ok {
		t.Fatal("ignored .20 must not be active")
	}
	// ReplaceManual must not resurrect a sticky-ignored address.
	if err := s.ReplaceManual(ctx, []string{"192.168.1.51", "192.168.1.20"}); err != nil {
		t.Fatalf("replace 3: %v", err)
	}
	active, _ = s.ListActive(ctx)
	if _, ok := addrs(active)["192.168.1.20"]; ok {
		t.Fatal("manual re-add resurrected a sticky-ignored device")
	}
}

// TestAdoptCap: at the cap, a fresh device is skipped, not added.
func TestAdoptCap(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		mac := string(rune('A'+i)) + "00000000000"
		addr := "192.168.1." + string(rune('0'+i))
		if _, err := s.Adopt(ctx, Detected{MAC: mac, Address: addr}, 3); err != nil {
			t.Fatalf("adopt %d: %v", i, err)
		}
	}
	res, err := s.Adopt(ctx, Detected{MAC: "Z00000000000", Address: "192.168.1.200"}, 3)
	if err != nil {
		t.Fatalf("adopt over cap: %v", err)
	}
	if res != AdoptSkippedFull {
		t.Fatalf("over-cap adopt = %v, want AdoptSkippedFull", res)
	}
	// A KNOWN device is still refreshed at the cap (not rejected).
	res, err = s.Adopt(ctx, Detected{MAC: "A00000000000", Address: "192.168.1.0"}, 3)
	if err != nil {
		t.Fatalf("adopt known at cap: %v", err)
	}
	if res != AdoptedKnown {
		t.Fatalf("known-at-cap = %v, want AdoptedKnown", res)
	}
}

// TestReleaseOnlyIgnored: releasing an active row is rejected.
func TestReleaseOnlyIgnored(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Adopt(ctx, Detected{MAC: "08F9E0E5C790", Address: "192.168.1.51"}, capN); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	active, _ := s.ListActive(ctx)
	if err := s.ReleaseByID(ctx, active[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("release active = %v, want ErrNotFound", err)
	}
	if err := s.RemoveByAddress(ctx, "10.0.0.1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remove missing = %v, want ErrNotFound", err)
	}
}

// TestAdoptDHCPMoveNoDuplicate: a device that DHCP-moves onto an address a
// stale active row still claims must not leave two active rows at one
// address (review finding #1).
func TestAdoptDHCPMoveNoDuplicate(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// A manual pin at A (no MAC) plus the same device discovered at B.
	if err := s.ReplaceManual(ctx, []string{"192.168.1.10"}); err != nil {
		t.Fatalf("manual: %v", err)
	}
	if _, err := s.Adopt(ctx, Detected{MAC: "08F9E0E5C790", Address: "192.168.1.11"}, capN); err != nil {
		t.Fatalf("adopt B: %v", err)
	}
	// Device DHCP-moves to A and announces there with its MAC.
	if _, err := s.Adopt(ctx, Detected{MAC: "08F9E0E5C790", Address: "192.168.1.10"}, capN); err != nil {
		t.Fatalf("adopt moved: %v", err)
	}
	active, _ := s.ListActive(ctx)
	countAt, macRows := 0, 0
	for _, d := range active {
		if d.Address == "192.168.1.10" {
			countAt++
		}
		if d.MAC == "08F9E0E5C790" {
			macRows++
		}
	}
	if countAt != 1 {
		t.Fatalf("active rows at moved address = %d, want 1 (duplicate!): %+v", countAt, active)
	}
	if macRows != 1 {
		t.Fatalf("rows for the device MAC = %d, want 1", macRows)
	}
}

// TestIgnoreDoesNotBlockDifferentDeviceAtSameIP: a removed device's IP,
// inherited by a genuinely DIFFERENT device (different MAC), must not block
// the new device (review finding #2).
func TestIgnoreDoesNotBlockDifferentDeviceAtSameIP(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// Remove device X (MAC MX) at address A.
	if _, err := s.Adopt(ctx, Detected{MAC: "AAAAAAAAAAAA", Address: "192.168.1.30"}, capN); err != nil {
		t.Fatalf("adopt X: %v", err)
	}
	if err := s.RemoveByAddress(ctx, "192.168.1.30"); err != nil {
		t.Fatalf("remove X: %v", err)
	}
	// A DIFFERENT device Y (MAC MY) inherits A's IP and announces.
	res, err := s.Adopt(ctx, Detected{MAC: "BBBBBBBBBBBB", Address: "192.168.1.30"}, capN)
	if err != nil {
		t.Fatalf("adopt Y: %v", err)
	}
	if res != AdoptedNew {
		t.Fatalf("adopt Y = %v, want AdoptedNew (a different device was wrongly blocked)", res)
	}
	// X itself (its MAC) is still sticky wherever it reappears.
	res, _ = s.Adopt(ctx, Detected{MAC: "AAAAAAAAAAAA", Address: "192.168.1.31"}, capN)
	if res != AdoptSkippedIgnored {
		t.Fatalf("re-adopt X at new addr = %v, want AdoptSkippedIgnored (MAC stickiness broke)", res)
	}
}

func keys(m map[string]Device) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
