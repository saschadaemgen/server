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

func newFakeSource() *fakeSource { return &fakeSource{ch: make(chan dnssd.Entry, 8)} }
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

func newDisco(t *testing.T, enabled *bool) (*shellyDiscovery, *shellystore.Store, *int) {
	t.Helper()
	store := newDiscoTestStore(t)
	src := newFakeSource()
	rebuilds := 0
	d := newShellyDiscovery(store, src, nil,
		func(context.Context) bool { return *enabled },
		func(context.Context) { rebuilds++ })
	return d, store, &rebuilds
}

// TestDiscoveryAutoAdopt: a discovered Gen2 device is adopted and triggers a
// client rebuild; a re-announcement does not re-trigger.
func TestDiscoveryAutoAdopt(t *testing.T) {
	on := true
	d, store, rebuilds := newDisco(t, &on)
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
	on := true
	d, store, _ := newDisco(t, &on)
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
	on := true
	d, store, rebuilds := newDisco(t, &on)
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

// TestDiscoveryGen1Rejected: a device announcing gen=1 is out of scope.
func TestDiscoveryGen1Rejected(t *testing.T) {
	on := true
	d, store, _ := newDisco(t, &on)
	ctx := context.Background()
	d.observe(ctx, entry("shelly1-112233445566", "192.168.1.70", 80, map[string]string{"gen": "1"}))
	if active, _ := store.ListActive(ctx); len(active) != 0 {
		t.Fatalf("active = %d, want 0 (gen1 adopted)", len(active))
	}
}

// TestDiscoveryDisabledNoAdopt: while the integration is off, nothing is
// adopted even though the announcement is valid.
func TestDiscoveryDisabledNoAdopt(t *testing.T) {
	on := false
	d, store, _ := newDisco(t, &on)
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
