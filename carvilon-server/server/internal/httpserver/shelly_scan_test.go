package httpserver

import (
	"context"
	"net/netip"
	"sort"
	"testing"
	"time"

	"carvilon.local/server/internal/shelly1api"
	"carvilon.local/server/internal/shellystore"
)

// ---- LAN guard + subnet planning (pure, the DoD's "guard unit-tested") ----

func mustPrefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatalf("parse prefix %q: %v", s, err)
	}
	return p
}

// own24Subnets keeps ONLY RFC1918 IPv4, masks to /24, and dedupes - the
// scan can never be aimed off the private LAN, and a /16 interface mask
// is still capped at /24.
func TestOwn24Subnets_RejectsNonPrivateAndCapsAt24(t *testing.T) {
	in := []netip.Prefix{
		mustPrefix(t, "192.168.1.10/24"),  // private -> 192.168.1.0/24
		mustPrefix(t, "10.0.5.9/16"),      // private, wide mask -> capped to 10.0.5.0/24
		mustPrefix(t, "172.16.4.4/20"),    // private -> 172.16.4.0/24
		mustPrefix(t, "127.0.0.1/8"),      // loopback -> dropped
		mustPrefix(t, "169.254.10.10/16"), // link-local -> dropped
		mustPrefix(t, "8.8.8.8/24"),       // public -> dropped
		mustPrefix(t, "192.168.1.50/24"),  // dup of the first /24 -> deduped
		mustPrefix(t, "2001:db8::1/64"),   // IPv6 -> dropped
	}
	got := own24Subnets(in)
	want := map[netip.Prefix]bool{
		mustPrefix(t, "192.168.1.0/24"): true,
		mustPrefix(t, "10.0.5.0/24"):    true,
		mustPrefix(t, "172.16.4.0/24"):  true,
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %d distinct /24s", got, len(want))
	}
	for _, p := range got {
		if p.Bits() != 24 {
			t.Errorf("subnet %v is not a /24", p)
		}
		if !want[p] {
			t.Errorf("unexpected subnet %v", p)
		}
	}
}

// planScanTargets yields exactly the .1-.254 hosts of each /24, no
// network/broadcast address, deduped across overlapping inputs.
func TestPlanScanTargets(t *testing.T) {
	targets := planScanTargets([]netip.Prefix{mustPrefix(t, "192.168.1.0/24")})
	if len(targets) != 254 {
		t.Fatalf("got %d targets, want 254 (.1-.254)", len(targets))
	}
	// never the network or broadcast address
	for _, a := range targets {
		last := a.As4()[3]
		if last == 0 || last == 255 {
			t.Fatalf("target %v includes a network/broadcast address", a)
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Less(targets[j]) })
	if targets[0].String() != "192.168.1.1" || targets[253].String() != "192.168.1.254" {
		t.Errorf("range = %v..%v", targets[0], targets[253])
	}
	// a non-/24 subnet contributes nothing (planner only sweeps /24s)
	if got := planScanTargets([]netip.Prefix{mustPrefix(t, "10.0.0.0/16")}); len(got) != 0 {
		t.Errorf("a /16 produced %d targets, want 0", len(got))
	}
}

// subnetContains is the last-line guard applied to every target.
func TestSubnetContains(t *testing.T) {
	subs := []netip.Prefix{mustPrefix(t, "192.168.1.0/24")}
	if !subnetContains(subs, netip.MustParseAddr("192.168.1.42")) {
		t.Error("in-subnet target rejected")
	}
	for _, off := range []string{"192.168.2.42", "10.0.0.1", "8.8.8.8"} {
		if subnetContains(subs, netip.MustParseAddr(off)) {
			t.Errorf("off-subnet target %s passed the guard", off)
		}
	}
}

// devScanSubnet accepts only RFC1918/loopback and always masks to /24.
func TestDevScanSubnet(t *testing.T) {
	cases := map[string]string{
		"192.168.1.10/24": "192.168.1.0/24",
		"127.0.0.1/24":    "127.0.0.0/24",
		"10.1.2.3/16":     "10.1.2.0/24", // capped
	}
	for in, want := range cases {
		got, ok := devScanSubnet(in)
		if !ok || got.String() != want {
			t.Errorf("devScanSubnet(%q) = %v,%v want %s", in, got, ok, want)
		}
	}
	for _, bad := range []string{"8.8.8.0/24", "169.254.0.0/16", "", "garbage", "2001:db8::/64"} {
		if _, ok := devScanSubnet(bad); ok {
			t.Errorf("devScanSubnet(%q) accepted an off-LAN/invalid target", bad)
		}
	}
}

// detectedFromScan classifies /shelly answers: a MAC is required, Gen1
// needs a supported type code, and a non-Shelly answer is rejected.
func TestDetectedFromScan(t *testing.T) {
	parse := func(raw string) *shelly1api.Identity {
		id, err := shelly1api.ParseIdentityForTest([]byte(raw))
		if err != nil {
			t.Fatalf("parse identity: %v", err)
		}
		return id
	}
	// Gen1 RGBW2 (the discoverable=false device the feature exists for)
	det, ok := detectedFromScan(parse(`{"type":"SHRGBW2","mac":"A4CF12C0FFEE","auth":false}`), "192.0.2.10")
	if !ok || det.Gen != shellystore.Gen1 || det.MAC != "A4CF12C0FFEE" ||
		det.Model != "SHRGBW2" || det.Origin != shellystore.OriginScanned || det.Name == "" {
		t.Fatalf("RGBW2 detect = %+v ok=%v", det, ok)
	}
	// Gen2
	det, ok = detectedFromScan(parse(`{"gen":2,"mac":"AABBCCDDEEFF","app":"Pro4PM"}`), "192.0.2.11")
	if !ok || det.Gen != shellystore.Gen2 || det.Model != "Shelly Pro4PM" {
		t.Fatalf("Gen2 detect = %+v ok=%v", det, ok)
	}
	// rejected: no MAC / unsupported Gen1 type / not a Shelly at all
	for _, raw := range []string{
		`{"type":"SHRGBW2","auth":false}`,        // no MAC
		`{"type":"SHDM-1","mac":"AABBCCDDEEFF"}`, // Gen1 dimmer - out of scope
		`{}`,                                     // random host
		`{"mac":"AABBCCDDEEFF"}`,                 // no gen, no type
	} {
		if _, ok := detectedFromScan(parse(raw), "192.0.2.12"); ok {
			t.Errorf("classified a non-in-scope answer as a device: %s", raw)
		}
	}
}

// ---- full pipeline: sweep -> classify -> dedupe -> approval gate ----

// TestScannerPipeline drives the scanner end to end with an injected
// prober against a synthetic /24, proving the DoD: a discoverable=false
// RGBW2 is found by scan and lands PENDING (never auto-adopted), tagged
// found-by-scan; the ignore list and MAC-dedupe are honoured; and no
// target outside the subnet is ever probed.
func TestScannerPipeline(t *testing.T) {
	store := newDiscoTestStore(t)
	ctx := context.Background()

	// One device already ignored (rejected earlier), one already active.
	if _, err := store.Adopt(ctx, shellystore.Detected{MAC: "1111AABBCCDD", Address: "192.168.9.20", Gen: shellystore.Gen1, Model: "SHSW-1"}, 32, false); err != nil {
		t.Fatal(err)
	}
	if pend, _ := store.ListPending(ctx); len(pend) == 1 {
		_ = store.RejectPending(ctx, pend[0].ID) // -> ignored
	}
	if _, err := store.Adopt(ctx, shellystore.Detected{MAC: "2222AABBCCDD", Address: "192.168.9.30", Gen: shellystore.Gen2, Model: "Shelly Plus1"}, 32, true); err != nil {
		t.Fatal(err)
	}

	// The synthetic LAN: an RGBW2 (the target), a Gen2, the already-ignored
	// device (must stay out), the already-active device (found, not new), a
	// non-Shelly host, and a Shelly with no MAC (rejected).
	answers := map[string]string{
		"192.168.9.10": `{"type":"SHRGBW2","mac":"A4CF12C0FFEE","auth":false,"discoverable":false}`,
		"192.168.9.11": `{"gen":2,"mac":"3333AABBCCDD","app":"Plus1PM"}`,
		"192.168.9.20": `{"type":"SHSW-1","mac":"1111AABBCCDD"}`,       // ignored - must be skipped
		"192.168.9.30": `{"gen":2,"mac":"2222AABBCCDD","app":"Plus1"}`, // already active
		"192.168.9.99": `{"server":"some other web thing"}`,            // not a Shelly
		"192.168.9.98": `{"type":"SHRGBW2"}`,                           // Shelly, no MAC -> rejected
	}
	var probedOffSubnet bool
	probe := func(_ context.Context, addr string) (*shelly1api.Identity, bool) {
		if !netip.MustParseAddr(addr).IsPrivate() {
			probedOffSubnet = true
		}
		raw, ok := answers[addr]
		if !ok {
			return nil, false // dead host
		}
		id, err := shelly1api.ParseIdentityForTest([]byte(raw))
		if err != nil {
			return nil, false
		}
		return id, true
	}

	sc := newShellyScanner(store, nil, func(context.Context) bool { return true }, probe, nil)
	sub := []netip.Prefix{mustPrefix(t, "192.168.9.0/24")}
	if !sc.Start(ctx, sub, scanPortDefault) {
		t.Fatal("scan did not start")
	}
	// wait for completion
	deadline := time.Now().Add(5 * time.Second)
	for {
		if p := sc.Snapshot(); !p.Running && p.Done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scan did not finish in time")
		}
		time.Sleep(10 * time.Millisecond)
	}

	prog := sc.Snapshot()
	if prog.Total != 254 || prog.Probed != 254 {
		t.Errorf("progress = probed %d / total %d, want 254/254", prog.Probed, prog.Total)
	}
	// found = RGBW2 + Gen2 + already-active (3 in-scope Shellies with a MAC;
	// the ignored one is skipped at Adopt, not at classify, so it counts as
	// "found" but not "new"). The no-MAC + non-Shelly are rejected.
	if prog.Found != 4 {
		t.Errorf("found = %d, want 4 (RGBW2, Gen2, ignored, active)", prog.Found)
	}
	if prog.New != 2 {
		t.Errorf("new = %d, want 2 (RGBW2 + the fresh Gen2)", prog.New)
	}
	if probedOffSubnet {
		t.Error("the scan probed an address outside its subnet")
	}

	// The RGBW2 is now PENDING (never auto-adopted) and tagged found-by-scan.
	pend, err := store.ListPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var rgbw *shellystore.Device
	for i := range pend {
		if pend[i].MAC == "A4CF12C0FFEE" {
			rgbw = &pend[i]
		}
	}
	if rgbw == nil {
		t.Fatalf("RGBW2 not pending after scan; pending = %+v", pend)
	}
	if rgbw.Origin != shellystore.OriginScanned || rgbw.State != shellystore.StatePending ||
		rgbw.Gen != shellystore.Gen1 || rgbw.Model != "SHRGBW2" {
		t.Errorf("RGBW2 row = %+v", *rgbw)
	}
	// the ignored device never re-entered the set
	for _, d := range pend {
		if d.MAC == "1111AABBCCDD" {
			t.Error("an ignored device was re-adopted by the scan")
		}
	}
}
