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

	res, err := s.Adopt(ctx, Detected{MAC: "08F9E0E5C790", Address: "192.168.1.51", Name: "Growbox", Model: "Shelly Pro4PM"}, capN, true)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if res != AdoptedNew {
		t.Fatalf("first adopt = %v, want AdoptedNew", res)
	}
	// Second announcement of the same device: known, not a duplicate.
	res, err = s.Adopt(ctx, Detected{MAC: "08F9E0E5C790", Address: "192.168.1.51", Name: "Growbox"}, capN, true)
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

	if _, err := s.Adopt(ctx, dev, capN, true); err != nil {
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
	res, err := s.Adopt(ctx, dev, capN, true)
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
	res, _ = s.Adopt(ctx, Detected{MAC: "08F9E0E5C790", Address: "192.168.1.99"}, capN, true)
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
	res, err = s.Adopt(ctx, dev, capN, true)
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
	res, err := s.Adopt(ctx, Detected{MAC: "AABBCCDDEEFF", Address: "192.168.1.60"}, capN, true)
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
	res, err := s.Adopt(ctx, Detected{MAC: "08F9E0E5C790", Address: "192.168.1.51", Name: "Growbox"}, capN, true)
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
	if _, err := s.Adopt(ctx, Detected{MAC: "111111111111", Address: "192.168.1.10"}, capN, true); err != nil {
		t.Fatalf("adopt disc: %v", err)
	}
	if _, err := s.Adopt(ctx, Detected{MAC: "222222222222", Address: "192.168.1.20"}, capN, true); err != nil {
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
		if _, err := s.Adopt(ctx, Detected{MAC: mac, Address: addr}, 3, true); err != nil {
			t.Fatalf("adopt %d: %v", i, err)
		}
	}
	res, err := s.Adopt(ctx, Detected{MAC: "Z00000000000", Address: "192.168.1.200"}, 3, true)
	if err != nil {
		t.Fatalf("adopt over cap: %v", err)
	}
	if res != AdoptSkippedFull {
		t.Fatalf("over-cap adopt = %v, want AdoptSkippedFull", res)
	}
	// A KNOWN device is still refreshed at the cap (not rejected).
	res, err = s.Adopt(ctx, Detected{MAC: "A00000000000", Address: "192.168.1.0"}, 3, true)
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
	if _, err := s.Adopt(ctx, Detected{MAC: "08F9E0E5C790", Address: "192.168.1.51"}, capN, true); err != nil {
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
	if _, err := s.Adopt(ctx, Detected{MAC: "08F9E0E5C790", Address: "192.168.1.11"}, capN, true); err != nil {
		t.Fatalf("adopt B: %v", err)
	}
	// Device DHCP-moves to A and announces there with its MAC.
	if _, err := s.Adopt(ctx, Detected{MAC: "08F9E0E5C790", Address: "192.168.1.10"}, capN, true); err != nil {
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
	if _, err := s.Adopt(ctx, Detected{MAC: "AAAAAAAAAAAA", Address: "192.168.1.30"}, capN, true); err != nil {
		t.Fatalf("adopt X: %v", err)
	}
	if err := s.RemoveByAddress(ctx, "192.168.1.30"); err != nil {
		t.Fatalf("remove X: %v", err)
	}
	// A DIFFERENT device Y (MAC MY) inherits A's IP and announces.
	res, err := s.Adopt(ctx, Detected{MAC: "BBBBBBBBBBBB", Address: "192.168.1.30"}, capN, true)
	if err != nil {
		t.Fatalf("adopt Y: %v", err)
	}
	if res != AdoptedNew {
		t.Fatalf("adopt Y = %v, want AdoptedNew (a different device was wrongly blocked)", res)
	}
	// X itself (its MAC) is still sticky wherever it reappears.
	res, _ = s.Adopt(ctx, Detected{MAC: "AAAAAAAAAAAA", Address: "192.168.1.31"}, capN, true)
	if res != AdoptSkippedIgnored {
		t.Fatalf("re-adopt X at new addr = %v, want AdoptSkippedIgnored (MAC stickiness broke)", res)
	}
}

// TestPendingGateDefault: with the gate on (autoAdopt=false) a discovered
// device lands as pending - not active, not polled - and can be approved
// into the active set.
func TestPendingGateDefault(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	dev := Detected{MAC: "08F9E0E5C790", Address: "192.168.1.51", Name: "Growbox"}

	res, err := s.Adopt(ctx, dev, capN, false)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if res != AdoptedPending {
		t.Fatalf("adopt = %v, want AdoptedPending", res)
	}
	if active, _ := s.ListActive(ctx); len(active) != 0 {
		t.Fatalf("active = %d, want 0 (pending must not be active)", len(active))
	}
	pending, _ := s.ListPending(ctx)
	if len(pending) != 1 || pending[0].MAC != "08F9E0E5C790" {
		t.Fatalf("pending = %+v, want the device", pending)
	}
	// Re-announcement keeps it pending (never silently activated).
	res, _ = s.Adopt(ctx, dev, capN, false)
	if res != AdoptedKnown {
		t.Fatalf("re-adopt = %v, want AdoptedKnown", res)
	}
	if active, _ := s.ListActive(ctx); len(active) != 0 {
		t.Fatalf("re-announce activated a pending device")
	}
	// Approve -> active.
	if err := s.ApprovePending(ctx, pending[0].ID, capN); err != nil {
		t.Fatalf("approve: %v", err)
	}
	active, _ := s.ListActive(ctx)
	if len(active) != 1 || active[0].Address != "192.168.1.51" {
		t.Fatalf("active after approve = %+v, want the device", active)
	}
	if p, _ := s.ListPending(ctx); len(p) != 0 {
		t.Fatalf("pending after approve = %d, want 0", len(p))
	}
}

// TestRejectPendingSticky: rejecting a pending device ignores it (sticky) so
// a re-announcement does not surface it again; release brings it back to
// pending.
func TestRejectPendingSticky(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	dev := Detected{MAC: "08F9E0E5C790", Address: "192.168.1.51"}

	if _, err := s.Adopt(ctx, dev, capN, false); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	pending, _ := s.ListPending(ctx)
	if err := s.RejectPending(ctx, pending[0].ID); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if p, _ := s.ListPending(ctx); len(p) != 0 {
		t.Fatalf("pending after reject = %d, want 0", len(p))
	}
	// Sticky: re-announce must not re-add (not even pending).
	res, _ := s.Adopt(ctx, dev, capN, false)
	if res != AdoptSkippedIgnored {
		t.Fatalf("re-adopt after reject = %v, want AdoptSkippedIgnored", res)
	}
	if p, _ := s.ListPending(ctx); len(p) != 0 {
		t.Fatalf("rejected device came back as pending")
	}
	// Release -> back to pending on next announcement.
	ign, _ := s.ListIgnored(ctx)
	if err := s.ReleaseByID(ctx, ign[0].ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	res, _ = s.Adopt(ctx, dev, capN, false)
	if res != AdoptedPending {
		t.Fatalf("adopt after release = %v, want AdoptedPending", res)
	}
}

// TestManualSupersedesPending: typing a pending device's address into the
// manual list activates it (and drops the pending row - no double state).
func TestManualSupersedesPending(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Adopt(ctx, Detected{MAC: "08F9E0E5C790", Address: "192.168.1.51"}, capN, false); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if err := s.ReplaceManual(ctx, []string{"192.168.1.51"}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if p, _ := s.ListPending(ctx); len(p) != 0 {
		t.Fatalf("pending after manual add = %d, want 0 (superseded)", len(p))
	}
	active, _ := s.ListActive(ctx)
	if len(active) != 1 || active[0].Origin != OriginManual {
		t.Fatalf("active = %+v, want one manual row", active)
	}
}

// TestPendingCap: the pending list is capped like the active set.
func TestPendingCap(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		mac := string(rune('A'+i)) + "00000000000"
		addr := "192.168.5." + string(rune('0'+i))
		if _, err := s.Adopt(ctx, Detected{MAC: mac, Address: addr}, 2, false); err != nil {
			t.Fatalf("adopt %d: %v", i, err)
		}
	}
	res, err := s.Adopt(ctx, Detected{MAC: "Z00000000000", Address: "192.168.5.200"}, 2, false)
	if err != nil {
		t.Fatalf("adopt over cap: %v", err)
	}
	if res != AdoptSkippedFull {
		t.Fatalf("over-cap pending adopt = %v, want AdoptSkippedFull", res)
	}
}

// TestPendingDoesNotEvictActive (review finding #1): an unapproved discovery
// (gate on) that lands on an approved active device's address must NOT delete
// the active device - approval is the operator's to revoke.
func TestPendingDoesNotEvictActive(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// Approved active device Y at A.
	if _, err := s.Adopt(ctx, Detected{MAC: "AAAAAAAAAAAA", Address: "192.168.1.40"}, capN, true); err != nil {
		t.Fatalf("adopt Y: %v", err)
	}
	// A DIFFERENT device X takes A's IP; gate on -> pending.
	res, err := s.Adopt(ctx, Detected{MAC: "BBBBBBBBBBBB", Address: "192.168.1.40"}, capN, false)
	if err != nil {
		t.Fatalf("adopt X: %v", err)
	}
	if res != AdoptedPending {
		t.Fatalf("adopt X = %v, want AdoptedPending", res)
	}
	// Y must still be active (not evicted by the unapproved find).
	active, _ := s.ListActive(ctx)
	if len(active) != 1 || active[0].MAC != "AAAAAAAAAAAA" {
		t.Fatalf("approved active device was evicted by a pending find: %+v", active)
	}
	pending, _ := s.ListPending(ctx)
	if len(pending) != 1 || pending[0].MAC != "BBBBBBBBBBBB" {
		t.Fatalf("pending = %+v, want X", pending)
	}
}

// TestApproveAtCapRejected (review finding #3): approving must not push the
// active set past the cap.
func TestApproveAtCapRejected(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	const limit = 2
	if _, err := s.Adopt(ctx, Detected{MAC: "A00000000000", Address: "192.168.2.1"}, limit, true); err != nil {
		t.Fatalf("adopt 1: %v", err)
	}
	if _, err := s.Adopt(ctx, Detected{MAC: "B00000000000", Address: "192.168.2.2"}, limit, true); err != nil {
		t.Fatalf("adopt 2: %v", err)
	}
	// A pending device at a third address (pending cap not hit).
	if _, err := s.Adopt(ctx, Detected{MAC: "C00000000000", Address: "192.168.2.3"}, limit, false); err != nil {
		t.Fatalf("adopt pending: %v", err)
	}
	pending, _ := s.ListPending(ctx)
	if err := s.ApprovePending(ctx, pending[0].ID, limit); !errors.Is(err, ErrAtCap) {
		t.Fatalf("approve at cap = %v, want ErrAtCap", err)
	}
	// It stays pending; active unchanged.
	if p, _ := s.ListPending(ctx); len(p) != 1 {
		t.Fatalf("pending after rejected approve = %d, want 1", len(p))
	}
	if a, _ := s.ListActive(ctx); len(a) != 2 {
		t.Fatalf("active after rejected approve = %d, want 2", len(a))
	}
	// Approving with the cap disabled (0) still works.
	if err := s.ApprovePending(ctx, pending[0].ID, 0); err != nil {
		t.Fatalf("approve uncapped: %v", err)
	}
}

// TestMQTTStateAndReaper: SetMQTTState records username+state; the startup
// reaper flips a stranded "provisioning" to "failed" and leaves others.
func TestMQTTStateAndReaper(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Adopt(ctx, Detected{MAC: "08F9E0E5C790", Address: "192.168.1.51"}, capN, true); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if _, err := s.Adopt(ctx, Detected{MAC: "08F9E0A11223", Address: "192.168.1.52"}, capN, true); err != nil {
		t.Fatalf("adopt2: %v", err)
	}
	active, _ := s.ListActive(ctx)
	// One device stuck "provisioning", one "linked".
	if err := s.SetMQTTState(ctx, active[0].ID, "shelly-08f9e0e5c790", MQTTStateProvisioning); err != nil {
		t.Fatalf("set provisioning: %v", err)
	}
	if err := s.SetMQTTState(ctx, active[1].ID, "shelly-08f9e0a11223", MQTTStateLinked); err != nil {
		t.Fatalf("set linked: %v", err)
	}
	// The reaper flips only the stranded one.
	if err := s.ResetStaleProvisioning(ctx); err != nil {
		t.Fatalf("reaper: %v", err)
	}
	got, _ := s.Get(ctx, active[0].ID)
	if got.MQTTState != MQTTStateFailed || got.MQTTUsername != "shelly-08f9e0e5c790" {
		t.Fatalf("stranded device = %q/%q, want failed + username kept", got.MQTTState, got.MQTTUsername)
	}
	got2, _ := s.Get(ctx, active[1].ID)
	if got2.MQTTState != MQTTStateLinked {
		t.Fatalf("linked device = %q, want unchanged", got2.MQTTState)
	}
	// The MQTT username survives a sticky removal (so the remove handler can
	// still find and delete the broker credential).
	if err := s.RemoveByAddress(ctx, "192.168.1.52"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	ign, _ := s.ListIgnored(ctx)
	if len(ign) != 1 || ign[0].MQTTUsername != "shelly-08f9e0a11223" {
		t.Fatalf("ignored row lost its mqtt username: %+v", ign)
	}
}

func keys(m map[string]Device) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
