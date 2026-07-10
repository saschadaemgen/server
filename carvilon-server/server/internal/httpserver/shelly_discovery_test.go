package httpserver

import (
	"context"
	"net/netip"
	"path/filepath"
	"testing"

	"carvilon.local/server/internal/db"
	"carvilon.local/server/internal/dnssd"
	"carvilon.local/server/internal/shellystore"
)

// fakeSource is a dnssd.Source that yields no passive entries (tests drive
// observe directly) and counts Scan calls.
type fakeSource struct {
	ch    chan dnssd.Entry
	scans int
}

func newFakeSource() *fakeSource                  { return &fakeSource{ch: make(chan dnssd.Entry, 8)} }
func (f *fakeSource) Entries() <-chan dnssd.Entry { return f.ch }
func (f *fakeSource) Scan(context.Context)        { f.scans++ }

func newDiscoTestStore(t *testing.T) *shellystore.Store {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return shellystore.New(d.DB)
}

// entry builds a synthetic Shelly announcement entry.
func entry(id, ip string, port uint16, txt map[string]string) dnssd.Entry {
	if txt == nil {
		txt = map[string]string{"gen": "2"}
	}
	return dnssd.Entry{
		Instance: id + "._shelly._tcp.local.",
		Service:  "_shelly._tcp.local.",
		Host:     id + ".local.",
		Addrs:    []netip.Addr{netip.MustParseAddr(ip)},
		Port:     port,
		TXT:      txt,
	}
}

func newDisco(t *testing.T, enabled, autoAdopt *bool) (*shellyDiscovery, *shellystore.Store, *int) {
	t.Helper()
	store := newDiscoTestStore(t)
	src := newFakeSource()
	rebuilds := 0
	d := newShellyDiscovery(store, src, nil, nil,
		func(context.Context) bool { return *enabled },
		func(context.Context) bool { return *autoAdopt },
		func(context.Context) { rebuilds++ })
	return d, store, &rebuilds
}

// TestDiscoveryPendingByDefault: with the approval gate on (default) a
// discovered device lands as pending - not active, not polled, no rebuild.
func TestDiscoveryPendingByDefault(t *testing.T) {
	on, auto := true, false
	d, store, rebuilds := newDisco(t, &on, &auto)
	ctx := context.Background()

	d.observe(ctx, entry("shellyplus1pm-a8032ab1c2d3", "192.168.1.51", 80, nil))
	if active, _ := store.ListActive(ctx); len(active) != 0 {
		t.Fatalf("active = %d, want 0 (gate on: device must be pending)", len(active))
	}
	pending, _ := store.ListPending(ctx)
	if len(pending) != 1 || pending[0].MAC != "A8032AB1C2D3" {
		t.Fatalf("pending = %+v, want the device", pending)
	}
	if *rebuilds != 0 {
		t.Fatalf("rebuilds = %d, want 0 (pending is not polled)", *rebuilds)
	}
	// Approving it activates it and rebuilds the fleet.
	if err := store.ApprovePending(ctx, pending[0].ID, 0); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if active, _ := store.ListActive(ctx); len(active) != 1 {
		t.Fatalf("active after approve = %d, want 1", len(active))
	}
}

// TestDiscoveryAutoAdoptWhenGateOff: with the gate off a discovered Gen2
// device is activated immediately and triggers a client rebuild; a
// re-announcement does not re-trigger.
func TestDiscoveryAutoAdoptWhenGateOff(t *testing.T) {
	on, auto := true, true
	d, store, rebuilds := newDisco(t, &on, &auto)
	ctx := context.Background()

	d.observe(ctx, entry("shellyplus1pm-a8032ab1c2d3", "192.168.1.51", 80, nil))
	active, _ := store.ListActive(ctx)
	if len(active) != 1 {
		t.Fatalf("active = %d, want 1", len(active))
	}
	if active[0].MAC != "A8032AB1C2D3" {
		t.Fatalf("mac = %q, want A8032AB1C2D3 (from instance label)", active[0].MAC)
	}
	if *rebuilds != 1 {
		t.Fatalf("rebuilds = %d, want 1", *rebuilds)
	}
	// Re-announce within the dedupe window: no new adoption, no rebuild.
	d.observe(ctx, entry("shellyplus1pm-a8032ab1c2d3", "192.168.1.51", 80, nil))
	if *rebuilds != 1 {
		t.Fatalf("rebuilds after re-announce = %d, want 1", *rebuilds)
	}
}

// TestDiscoveryStickyRemoval: the coordinator never re-adopts a device that
// was removed (ignore list), even across the dedupe window.
func TestDiscoveryStickyRemoval(t *testing.T) {
	on, auto := true, true
	d, store, _ := newDisco(t, &on, &auto)
	ctx := context.Background()

	d.observe(ctx, entry("shellypro4pm-08f9e0e5c790", "192.168.1.52", 80, nil))
	if err := store.RemoveByAddress(ctx, "192.168.1.52"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	// Force past the dedupe window by clearing the in-memory cache.
	d.resetDedupeForTest()

	d.observe(ctx, entry("shellypro4pm-08f9e0e5c790", "192.168.1.52", 80, nil))
	if active, _ := store.ListActive(ctx); len(active) != 0 {
		t.Fatalf("active after re-announce of removed device = %d, want 0 (STICKY FAILED)", len(active))
	}
}

// TestDiscoveryOffLANRejected: an announcement carrying a public / off-LAN
// address is dropped - it must never inject a foreign dial target.
func TestDiscoveryOffLANRejected(t *testing.T) {
	on, auto := true, true
	d, store, rebuilds := newDisco(t, &on, &auto)
	ctx := context.Background()

	// Public, metadata, loopback and link-local are all rejected on the
	// UNTRUSTED discovery path (stricter than the manual list): a hostile
	// announcement must not make us auto-dial localhost or a link-local box.
	for _, ip := range []string{"8.8.8.8", "1.2.3.4", "169.254.169.254", "127.0.0.1", "169.254.10.20"} {
		d.observe(ctx, entry("shellyevil-aabbccddeeff", ip, 80, nil))
		d.resetDedupeForTest()
	}
	if active, _ := store.ListActive(ctx); len(active) != 0 {
		t.Fatalf("active = %d, want 0 (off-LAN/loopback address adopted!)", len(active))
	}
	if *rebuilds != 0 {
		t.Fatalf("rebuilds = %d, want 0", *rebuilds)
	}
}

// TestDiscoveryGateToggleAffectsNewFinds: flipping the gate at runtime
// changes only NEW finds - an existing pending device is not activated, and
// a fresh find under auto-adopt goes straight to active.
func TestDiscoveryGateToggleAffectsNewFinds(t *testing.T) {
	on, auto := true, false // gate on
	d, store, _ := newDisco(t, &on, &auto)
	ctx := context.Background()

	// Device A found under the gate -> pending.
	d.observe(ctx, entry("shellyplus1pm-a8032ab1c2d3", "192.168.1.51", 80, nil))
	if p, _ := store.ListPending(ctx); len(p) != 1 {
		t.Fatalf("A pending = %d, want 1", len(p))
	}

	// Operator turns auto-adopt on.
	auto = true

	// Device B found under auto-adopt -> active immediately.
	d.observe(ctx, entry("shellypro4pm-08f9e0b44556", "192.168.1.52", 80, nil))
	if active, _ := store.ListActive(ctx); len(active) != 1 || active[0].Address != "192.168.1.52" {
		t.Fatalf("B should be active; got %+v", active)
	}
	// A stays pending - the toggle does not mass-activate existing pendings.
	if p, _ := store.ListPending(ctx); len(p) != 1 || p[0].Address != "192.168.1.51" {
		t.Fatalf("A must remain pending; got %+v", p)
	}
}

// TestDiscoveryDedupeMapBounded: a flood of DISTINCT announced identities
// must not grow the in-memory dedupe map without bound (memory flood guard).
func TestDiscoveryDedupeMapBounded(t *testing.T) {
	on, auto := true, false
	d, _, _ := newDisco(t, &on, &auto)
	ctx := context.Background()
	// Many distinct identities (distinct MACs) all at one LAN address, so the
	// DB stays at ~1 row (each supersedes the last at that address) while the
	// dedupe map is hammered with distinct keys - far more than the cap.
	for i := 0; i < dedupeMaxEntries*3; i++ {
		mac := normalizeMAC("02" + hex10(i))
		d.observe(ctx, dnssd.Entry{
			Instance: "shellyx-" + mac + "._shelly._tcp.local.",
			Service:  "_shelly._tcp.local.",
			TXT:      map[string]string{"gen": "2", "mac": mac},
			Addrs:    []netip.Addr{netip.MustParseAddr("10.0.0.1")},
			Port:     80,
		})
	}
	d.mu.Lock()
	n := len(d.lastSeen)
	d.mu.Unlock()
	if n > dedupeMaxEntries {
		t.Fatalf("lastSeen map grew to %d, want <= %d (unbounded flood!)", n, dedupeMaxEntries)
	}
}

// hex10 renders i as a 10-hex-digit string (pairs with a 2-digit prefix to
// make a 12-hex MAC).
func hex10(i int) string {
	const d = "0123456789ABCDEF"
	b := make([]byte, 10)
	for j := 9; j >= 0; j-- {
		b[j] = d[i&0xf]
		i >>= 4
	}
	return string(b)
}

// TestDiscoveryGen1Rejected: a device announcing gen=1 is out of scope.
func TestDiscoveryGen1Rejected(t *testing.T) {
	on, auto := true, true
	d, store, _ := newDisco(t, &on, &auto)
	ctx := context.Background()
	d.observe(ctx, entry("shelly1-112233445566", "192.168.1.70", 80, map[string]string{"gen": "1"}))
	if active, _ := store.ListActive(ctx); len(active) != 0 {
		t.Fatalf("active = %d, want 0 (gen1 adopted)", len(active))
	}
}

// TestDiscoveryDisabledNoAdopt: while the integration is off, nothing is
// adopted even though the announcement is valid.
func TestDiscoveryDisabledNoAdopt(t *testing.T) {
	on, auto := false, true
	d, store, _ := newDisco(t, &on, &auto)
	ctx := context.Background()
	d.observe(ctx, entry("shellyplus1pm-a8032ab1c2d3", "192.168.1.51", 80, nil))
	if active, _ := store.ListActive(ctx); len(active) != 0 {
		t.Fatalf("active while disabled = %d, want 0", len(active))
	}
	on = true // re-enable and the same announcement now adopts
	d.resetDedupeForTest()
	d.observe(ctx, entry("shellyplus1pm-a8032ab1c2d3", "192.168.1.51", 80, nil))
	if active, _ := store.ListActive(ctx); len(active) != 1 {
		t.Fatalf("active after enable = %d, want 1", len(active))
	}
}

func TestShellyDetectedFromEntry(t *testing.T) {
	// Address canonicalisation: default port folds to bare host.
	det, ok := shellyDetectedFromEntry(entry("shellyplus1pm-a8032ab1c2d3", "192.168.1.51", 80,
		map[string]string{"gen": "2", "app": "Plus1PM"}))
	if !ok {
		t.Fatal("want ok")
	}
	if det.Address != "192.168.1.51" {
		t.Fatalf("address = %q, want bare host", det.Address)
	}
	if det.Model != "Shelly Plus1PM" {
		t.Fatalf("model = %q", det.Model)
	}
	// Non-default port is kept.
	det, _ = shellyDetectedFromEntry(entry("shellyplus1pm-a8032ab1c2d3", "192.168.1.51", 8080,
		map[string]string{"gen": "2"}))
	if det.Address != "192.168.1.51:8080" {
		t.Fatalf("address = %q, want host:8080", det.Address)
	}
	// mac TXT wins over the label.
	det, _ = shellyDetectedFromEntry(entry("weirdlabel", "10.0.0.5", 80,
		map[string]string{"gen": "2", "mac": "AA:BB:CC:DD:EE:FF"}))
	if det.MAC != "AABBCCDDEEFF" {
		t.Fatalf("mac = %q, want AABBCCDDEEFF", det.MAC)
	}
}

func TestNormalizeMAC(t *testing.T) {
	cases := map[string]string{
		"a8:03:2a:b1:c2:d3": "A8032AB1C2D3",
		"A8032AB1C2D3":      "A8032AB1C2D3",
		"a8-03-2a-b1-c2-d3": "A8032AB1C2D3",
		"tooshort":          "",
		"":                  "",
		"zzzzzzzzzzzz":      "",
		"a8032ab1c2d3ff":    "", // 14 hex digits, not a 48-bit MAC
	}
	for in, want := range cases {
		if got := normalizeMAC(in); got != want {
			t.Errorf("normalizeMAC(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestShellyMQTTUsername: the broker account name is stable, lowercase and
// within the broker's username charset, from MAC (preferred) or address.
func TestShellyMQTTUsername(t *testing.T) {
	cases := []struct{ storedMAC, liveMAC, addr, want string }{
		{"08F9E0E5C790", "", "192.168.1.51", "shelly-08f9e0e5c790"},
		{"", "A8:03:2A:B1:C2:D3", "192.168.1.52", "shelly-a8032ab1c2d3"},
		{"", "", "192.168.1.53:8080", "shelly-192.168.1.53-8080"}, // no MAC -> sanitized address
	}
	for _, c := range cases {
		got := shellyMQTTUsername(c.storedMAC, c.liveMAC, c.addr)
		if got != c.want {
			t.Errorf("shellyMQTTUsername(%q,%q,%q) = %q, want %q", c.storedMAC, c.liveMAC, c.addr, got, c.want)
		}
	}
}
