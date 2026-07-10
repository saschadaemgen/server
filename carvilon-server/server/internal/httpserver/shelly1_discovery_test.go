package httpserver

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"carvilon.local/server/internal/dnssd"
	"carvilon.local/server/internal/shellystore"
)

// gen1Entry builds a synthetic Gen1 _http._tcp announcement (Gen1 devices
// carry no TXT identity; everything is in the instance name).
func gen1Entry(label, ip string, port uint16) dnssd.Entry {
	return dnssd.Entry{
		Instance: label + "." + Shelly1ServiceType + ".",
		Service:  Shelly1ServiceType + ".",
		Host:     label + ".local.",
		Addrs:    []netip.Addr{netip.MustParseAddr(ip)},
		Port:     port,
	}
}

// TestShelly1DetectedFromEntry_Admits: every documented relay-class name
// shape classifies to its type code with Gen=1; the MAC is kept only when
// the tail is a full 12-hex device id (longid=1), never a 3-byte tail.
func TestShelly1DetectedFromEntry_Admits(t *testing.T) {
	cases := []struct {
		label     string
		wantMAC   string
		wantModel string
	}{
		// longid=0 firmware: the 3-byte tail is NOT a usable MAC, so the
		// find must be keyed by address (MAC "") - never a fake identity.
		{"shelly1-B929CC", "", "SHSW-1"},
		// longid=1 firmware: the tail is the full MAC.
		{"shellyswitch25-A4CF12E4B7C1", "A4CF12E4B7C1", "SHSW-25"},
		// Longest-prefix rule: "shellyplug-s-" must win over "shellyplug"
		// (SHPLG-1), or every Plug S would be misfiled.
		{"shellyplug-s-AABBCC", "", "SHPLG-S"},
		// mDNS names are case-insensitive; classification must be too.
		{"SHELLY1PM-aabbccddeeff", "AABBCCDDEEFF", "SHSW-PM"},
	}
	for _, c := range cases {
		det, ok := shelly1DetectedFromEntry(gen1Entry(c.label, "192.168.1.60", 80))
		if !ok {
			t.Errorf("%q: not admitted, want ok", c.label)
			continue
		}
		if det.MAC != c.wantMAC {
			t.Errorf("%q: mac = %q, want %q", c.label, det.MAC, c.wantMAC)
		}
		if det.Model != c.wantModel {
			t.Errorf("%q: model = %q, want %q", c.label, det.Model, c.wantModel)
		}
		if det.Gen != shellystore.Gen1 {
			t.Errorf("%q: gen = %d, want %d", c.label, det.Gen, shellystore.Gen1)
		}
		if det.Address != "192.168.1.60" {
			t.Errorf("%q: address = %q, want bare host", c.label, det.Address)
		}
		if det.Name != c.label {
			t.Errorf("%q: name = %q, want the original label", c.label, det.Name)
		}
	}
}

// TestShelly1DetectedFromEntry_Rejects: _http._tcp hears everything on the
// LAN, so anything not on the strict Gen1 relay-class allowlist is dropped -
// especially Gen2+ names (the same physical device must not enter once per
// generation) and out-of-scope Gen1 models.
func TestShelly1DetectedFromEntry_Rejects(t *testing.T) {
	for _, label := range []string{
		"shellyplus1pm-aabbccddeeff", // Gen2+: owned by the _shelly._tcp path
		"shellypro4pm-08f9e0e5c790",  // Gen2+: same
		"shellydimmer-aabbccddeeff",  // Gen1 but out of the relay-class scope
		"printer-office",             // non-Shelly web thing on the LAN
		"shelly1",                    // no "-<id>" separator: not a device name
	} {
		if _, ok := shelly1DetectedFromEntry(gen1Entry(label, "192.168.1.60", 80)); ok {
			t.Errorf("%q admitted, want rejected", label)
		}
	}
}

// TestShelly1DetectedFromEntry_NoLANAddress: a valid Gen1 name with no
// RFC1918 IPv4 is rejected - the discovery trust rule is the same as Gen2
// (a hostile announcement must not inject a loopback/public dial target).
func TestShelly1DetectedFromEntry_NoLANAddress(t *testing.T) {
	for _, ip := range []string{"8.8.8.8", "127.0.0.1", "169.254.10.20"} {
		if _, ok := shelly1DetectedFromEntry(gen1Entry("shelly1-B929CC", ip, 80)); ok {
			t.Errorf("ip %s admitted, want rejected (not RFC1918)", ip)
		}
	}
}

// TestDiscoveryGen1PendingByDefault: a Gen1 find runs through the SAME
// approval gate as Gen2 - it lands pending (not active, not polled) and is
// stored with Gen=1 so approval provisions it down the Gen1 path.
func TestDiscoveryGen1PendingByDefault(t *testing.T) {
	on, auto := true, false
	d, store, rebuilds := newDisco(t, &on, &auto)
	ctx := context.Background()

	d.observeGen1(ctx, gen1Entry("shellyswitch25-a4cf12e4b7c1", "192.168.1.61", 80))
	if active, _ := store.ListActive(ctx); len(active) != 0 {
		t.Fatalf("active = %d, want 0 (gate on: device must be pending)", len(active))
	}
	pending, _ := store.ListPending(ctx)
	if len(pending) != 1 {
		t.Fatalf("pending = %+v, want the device", pending)
	}
	if pending[0].Gen != shellystore.Gen1 {
		t.Fatalf("gen = %d, want %d", pending[0].Gen, shellystore.Gen1)
	}
	if pending[0].MAC != "A4CF12E4B7C1" || pending[0].Model != "SHSW-25" {
		t.Fatalf("pending = %+v, want MAC A4CF12E4B7C1 / model SHSW-25", pending[0])
	}
	if *rebuilds != 0 {
		t.Fatalf("rebuilds = %d, want 0 (pending is not polled)", *rebuilds)
	}
}

// TestDiscoveryGen1AutoAdoptWhenGateOff: with the gate off a Gen1 find is
// activated immediately (Gen=1 in the row) and triggers a fleet rebuild.
func TestDiscoveryGen1AutoAdoptWhenGateOff(t *testing.T) {
	on, auto := true, true
	d, store, rebuilds := newDisco(t, &on, &auto)
	ctx := context.Background()

	d.observeGen1(ctx, gen1Entry("shelly1pm-aabbccddeeff", "192.168.1.62", 80))
	active, _ := store.ListActive(ctx)
	if len(active) != 1 {
		t.Fatalf("active = %d, want 1", len(active))
	}
	if active[0].Gen != shellystore.Gen1 {
		t.Fatalf("gen = %d, want %d", active[0].Gen, shellystore.Gen1)
	}
	if *rebuilds != 1 {
		t.Fatalf("rebuilds = %d, want 1", *rebuilds)
	}
}

// TestDiscoveryGen1StickyRemoval: a removed Gen1 device is never re-adopted
// on re-announcement (ignore list). Uses a short-id device (no MAC) so the
// address-keyed identity honours the list too.
func TestDiscoveryGen1StickyRemoval(t *testing.T) {
	on, auto := true, true
	d, store, _ := newDisco(t, &on, &auto)
	ctx := context.Background()

	d.observeGen1(ctx, gen1Entry("shelly1-B929CC", "192.168.1.63", 80))
	if active, _ := store.ListActive(ctx); len(active) != 1 {
		t.Fatalf("active = %d, want 1 before removal", len(active))
	}
	if err := store.RemoveByAddress(ctx, "192.168.1.63"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	// Force past the dedupe window by clearing the in-memory cache.
	d.resetDedupeForTest()

	d.observeGen1(ctx, gen1Entry("shelly1-B929CC", "192.168.1.63", 80))
	if active, _ := store.ListActive(ctx); len(active) != 0 {
		t.Fatalf("active after re-announce of removed device = %d, want 0 (STICKY FAILED)", len(active))
	}
}

// TestDiscoveryGen1Gen2NameNotAdopted: a Gen2+ instance name heard over the
// Gen1 wire must not enter through observeGen1 either - the coordinator-
// level guarantee that one physical device cannot enter twice.
func TestDiscoveryGen1Gen2NameNotAdopted(t *testing.T) {
	on, auto := true, true
	d, store, _ := newDisco(t, &on, &auto)
	ctx := context.Background()

	d.observeGen1(ctx, gen1Entry("shellyplus1pm-a8032ab1c2d3", "192.168.1.64", 80))
	if active, _ := store.ListActive(ctx); len(active) != 0 {
		t.Fatalf("active = %d, want 0 (Gen2 name adopted via the Gen1 path)", len(active))
	}
}

// TestDiscoveryRunNilGen1Source: Run with a nil gen1Source must degrade to
// the Gen2-only behaviour (a nil channel arm never fires) - no panic, the
// Gen2 stream still lands, and cancellation still stops the loop.
func TestDiscoveryRunNilGen1Source(t *testing.T) {
	store := newDiscoTestStore(t)
	src := newFakeSource()
	on, auto := true, false
	d := newShellyDiscovery(store, src, nil, nil,
		func(context.Context) bool { return on },
		func(context.Context) bool { return auto },
		func(context.Context) {})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(done)
	}()

	src.ch <- entry("shellyplus1pm-a8032ab1c2d3", "192.168.1.65", 80, nil)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if p, _ := store.ListPending(context.Background()); len(p) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Gen2 entry never landed while gen1Source is nil")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestShellyGenFromTXT: the _shelly._tcp service is Gen2+ by definition, so
// an absent or unparseable gen records as the floor (2); a parsed value is
// kept verbatim.
func TestShellyGenFromTXT(t *testing.T) {
	cases := []struct {
		txt  map[string]string
		want int
	}{
		{nil, 2},                                // absent map
		{map[string]string{}, 2},                // absent key
		{map[string]string{"gen": "potato"}, 2}, // garbage
		{map[string]string{"gen": "3"}, 3},      // verbatim
	}
	for _, c := range cases {
		if got := shellyGenFromTXT(c.txt); got != c.want {
			t.Errorf("shellyGenFromTXT(%v) = %d, want %d", c.txt, got, c.want)
		}
	}
}
