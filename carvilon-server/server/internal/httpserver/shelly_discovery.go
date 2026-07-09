// Shelly Etappe 2 - mDNS auto-discovery coordinator. It sits between the
// generic dnssd browser (the wire) and the shelly_devices store (the set),
// applying the trust + adoption policy: only Gen2+ instances, only LAN
// IPv4 addresses (the same guard the manual list uses), auto-adopt unless
// the device is on the sticky ignore list, and never more than the cap.
//
// Everything here is read-only toward the device: adoption and removal are
// CARVILON-side config actions. Identities (MAC, address) NEVER reach a log
// line - only coarse, count-level events do, so the admin System Log stays
// clean of device fingerprints.
package httpserver

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"carvilon.local/server/internal/dnssd"
	"carvilon.local/server/internal/shellystore"
)

// handleAdminShellyDevAnnounce (DevMode ONLY) feeds a synthetic mDNS
// announcement through the real discovery/adoption path, so the auto-adopt
// -> sticky-remove -> release chain can be exercised deterministically in
// dev without a live device or working OS multicast. The route is only
// registered when cfg.DevMode is set; this re-check is defence in depth.
//
// Form: ip (required, LAN IPv4), port (default 80), mac|id (optional device
// id), app (optional model slug), gen (optional). The entry then runs the
// SAME observe() policy as a real find - the LAN guard, cap and ignore list
// all apply, so it is not a bypass of any rule.
func (s *Server) handleAdminShellyDevAnnounce(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.DevMode || s.shellyDisco == nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	ipStr := strings.TrimSpace(r.PostForm.Get("ip"))
	ip, err := netip.ParseAddr(ipStr)
	if err != nil || !ip.Is4() {
		http.Error(w, "ip must be an IPv4 address", http.StatusBadRequest)
		return
	}
	port := uint16(80)
	if p := strings.TrimSpace(r.PostForm.Get("port")); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			http.Error(w, "invalid port", http.StatusBadRequest)
			return
		}
		port = uint16(n)
	}
	id := strings.TrimSpace(r.PostForm.Get("mac"))
	if id == "" {
		id = strings.TrimSpace(r.PostForm.Get("id"))
	}
	label := id
	if label == "" {
		label = "shellyunknown"
	}
	txt := map[string]string{}
	if g := strings.TrimSpace(r.PostForm.Get("gen")); g != "" {
		txt["gen"] = g
	}
	if app := strings.TrimSpace(r.PostForm.Get("app")); app != "" {
		txt["app"] = app
	}
	if id != "" {
		txt["id"] = id
	}
	entry := dnssd.Entry{
		Instance: label + "." + ShellyServiceType + ".",
		Service:  ShellyServiceType + ".",
		Host:     label + ".local.",
		Addrs:    []netip.Addr{ip},
		Port:     port,
		TXT:      txt,
	}
	s.shellyDisco.InjectForTest(r.Context(), entry)
	http.Redirect(w, r, "/a/devices", http.StatusSeeOther)
}

// ShellyServiceType is the DNS-SD service Gen2+ Shelly devices advertise.
// Gen1 devices announce differently and are a later, separate adapter.
// Exported so main can open the dnssd browser for it.
const ShellyServiceType = "_shelly._tcp.local"

const (
	// discoveryBackstop is the slow periodic active-query interval - the
	// safety net for a missed passive announcement. Deliberately long so the
	// feature stays near-zero load; the passive listener does the real work.
	discoveryBackstop = 4 * time.Minute
	// discoveryDedupeWindow throttles repeat processing of the same identity
	// (announcements recur): at most one store touch per identity per window,
	// which also blunts an announcement flood.
	discoveryDedupeWindow = 20 * time.Second
	// capLogInterval rate-limits the "at cap" warning so a flood cannot spam
	// the log.
	capLogInterval = time.Minute
	// dedupeMaxEntries bounds the in-memory recently-seen map so an
	// announcement flood of distinct identities cannot exhaust memory.
	// Generous headroom over any real LAN's device count.
	dedupeMaxEntries = 4096
)

// shellyDiscovery consumes a dnssd source and reconciles finds into the
// store. It is created once and run for the process lifetime.
type shellyDiscovery struct {
	store     *shellystore.Store
	source    dnssd.Source
	log       *slog.Logger
	enabled   func(context.Context) bool // Shelly integration on? (adoption gate)
	autoAdopt func(context.Context) bool // approval gate off? (auto-activate finds)
	rebuild   func(context.Context)      // rebuild the live client fleet
	limit     int
	now       func() time.Time

	mu         sync.Mutex
	lastSeen   map[string]time.Time
	lastCapLog time.Time

	// scanning coalesces active scans: at most one runs at a time, so a
	// burst of "Scan now" clicks (or a backstop tick landing on a manual
	// scan) cannot pile up goroutines / ephemeral sockets.
	scanning atomic.Bool

	closeOnce sync.Once
	closed    chan struct{}
}

// newShellyDiscovery wires the coordinator. source may be a *dnssd.Browser
// or a test fake; enabled/autoAdopt/rebuild are Server methods.
func newShellyDiscovery(store *shellystore.Store, source dnssd.Source, log *slog.Logger, enabled, autoAdopt func(context.Context) bool, rebuild func(context.Context)) *shellyDiscovery {
	if log == nil {
		log = slog.Default()
	}
	return &shellyDiscovery{
		store:     store,
		source:    source,
		log:       log,
		enabled:   enabled,
		autoAdopt: autoAdopt,
		rebuild:   rebuild,
		limit:     maxShellyAddresses,
		now:       time.Now,
		lastSeen:  map[string]time.Time{},
		closed:    make(chan struct{}),
	}
}

// Run consumes announcements and drives the periodic backstop until ctx is
// cancelled. Blocking; start it in its own goroutine.
func (d *shellyDiscovery) Run(ctx context.Context) {
	if d == nil || d.source == nil {
		return
	}
	// An initial active scan so a fresh start finds already-present devices
	// without waiting for the next backstop tick.
	d.ScanNow()

	ticker := time.NewTicker(discoveryBackstop)
	defer ticker.Stop()
	entries := d.source.Entries()
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.closed:
			return
		case e := <-entries:
			d.observe(ctx, e)
		case <-ticker.C:
			if d.enabled(ctx) {
				d.ScanNow()
			}
		}
	}
}

// Close stops Run. Idempotent; nil-safe.
func (d *shellyDiscovery) Close() {
	if d == nil {
		return
	}
	d.closeOnce.Do(func() { close(d.closed) })
}

// ScanNow provokes one active mDNS query on its own short-lived context so
// it survives the triggering HTTP request. A no-op while the integration is
// disabled (no chatter for an off feature).
func (d *shellyDiscovery) ScanNow() {
	if d == nil || d.source == nil {
		return
	}
	// Coalesce: skip when a scan is already in flight (a re-scan while one
	// runs would find the same devices anyway).
	if !d.scanning.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer d.scanning.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if d.enabled != nil && !d.enabled(ctx) {
			return
		}
		d.source.Scan(ctx)
	}()
}

// observe applies the adoption policy to one seen instance. It is the trust
// boundary: an off-LAN address or a non-Shelly/Gen1 instance is dropped
// here, and a device on the ignore list is never re-adopted.
func (d *shellyDiscovery) observe(ctx context.Context, e dnssd.Entry) {
	if d.enabled == nil || !d.enabled(ctx) {
		return // integration off: hear nothing, adopt nothing
	}
	det, ok := shellyDetectedFromEntry(e)
	if !ok {
		return // not a usable Gen2+ Shelly, or no LAN address (off-LAN rejected)
	}

	// Dedupe by durable identity so a chatty device (or a flood) does not
	// hammer the store; the key is the MAC when known, else the address.
	key := det.MAC
	if key == "" {
		key = "addr:" + det.Address
	}
	now := d.now()
	d.mu.Lock()
	if last, seen := d.lastSeen[key]; seen && now.Sub(last) < discoveryDedupeWindow {
		d.mu.Unlock()
		return
	}
	// Bound the map so an announcement flood of DISTINCT identities cannot
	// grow it without limit (the DB rows and the log are already capped/rate-
	// limited; this map was the last unbounded dimension). At the soft cap,
	// drop entries older than the dedupe window (useless anyway); if a within-
	// window flood still holds it at the cap, clear it entirely - the dedupe
	// is only an optimisation, so correctness is unaffected and memory stays
	// bounded.
	if len(d.lastSeen) >= dedupeMaxEntries {
		for k, t := range d.lastSeen {
			if now.Sub(t) >= discoveryDedupeWindow {
				delete(d.lastSeen, k)
			}
		}
		if len(d.lastSeen) >= dedupeMaxEntries {
			d.lastSeen = make(map[string]time.Time)
		}
	}
	d.lastSeen[key] = now
	d.mu.Unlock()

	autoAdopt := d.autoAdopt != nil && d.autoAdopt(ctx)
	res, err := d.store.Adopt(ctx, det, d.limit, autoAdopt)
	if err != nil {
		d.log.Warn("shelly discovery: adopt failed", "component", "shelly-discovery", "err", err)
		return
	}
	switch res {
	case shellystore.AdoptedNew:
		// Gate off: activated immediately. No identity in the log.
		d.log.Info("shelly device auto-adopted via mdns", "component", "shelly-discovery")
		if d.rebuild != nil {
			d.rebuild(ctx)
		}
	case shellystore.AdoptedPending:
		// Gate on (default): recorded, awaiting approval. NOT polled, so no
		// fleet rebuild. Identity-free log.
		d.log.Info("shelly device discovered, pending approval", "component", "shelly-discovery")
	case shellystore.AdoptSkippedFull:
		d.mu.Lock()
		spam := d.now().Sub(d.lastCapLog) < capLogInterval
		if !spam {
			d.lastCapLog = d.now()
		}
		d.mu.Unlock()
		if !spam {
			d.log.Warn("shelly discovery at device cap; ignoring further finds",
				"component", "shelly-discovery", "cap", d.limit)
		}
	case shellystore.AdoptSkippedIgnored, shellystore.AdoptedKnown:
		// Expected steady state - silent.
	}
}

// InjectForTest feeds one synthetic entry through the exact observe() path,
// for the dev-only announce endpoint and tests. Never used in production.
func (d *shellyDiscovery) InjectForTest(ctx context.Context, e dnssd.Entry) {
	d.observe(ctx, e)
}

// resetDedupeForTest clears the in-memory recently-seen cache so a test can
// re-present the same identity without waiting out the dedupe window.
func (d *shellyDiscovery) resetDedupeForTest() {
	d.mu.Lock()
	d.lastSeen = map[string]time.Time{}
	d.mu.Unlock()
}

// shellyDetectedFromEntry turns a dnssd entry into the store's Detected
// shape, applying every trust rule:
//
//   - Gen filter: a "gen" TXT that parses to < 2 is rejected (Gen1 is out of
//     scope); an absent gen is allowed (the _shelly._tcp service is Gen2+).
//   - Address: the first A record that passes the LAN guard, canonicalised
//     to the dial form. No LAN address -> not ok (an off-LAN announcement
//     cannot inject a foreign target).
//   - Identity: the MAC from the "mac"/"id" TXT or the instance label, kept
//     only when it is a clean 12-hex-digit value; otherwise "".
//
// It is pure (no I/O), so the policy is unit-tested directly.
func shellyDetectedFromEntry(e dnssd.Entry) (shellystore.Detected, bool) {
	if !shellyGenOK(e.TXT) {
		return shellystore.Detected{}, false
	}
	addr, ok := shellyAddrFromEntry(e)
	if !ok {
		return shellystore.Detected{}, false
	}
	return shellystore.Detected{
		MAC:     shellyMACFromEntry(e),
		Address: addr,
		Name:    e.InstanceLabel(),
		Model:   shellyModelFromEntry(e),
	}, true
}

// shellyGenOK reports whether the announced generation is 2 or newer (or
// simply absent, which the _shelly._tcp service already implies).
func shellyGenOK(txt map[string]string) bool {
	g, ok := txt["gen"]
	if !ok || strings.TrimSpace(g) == "" {
		return true
	}
	n, err := strconv.Atoi(strings.TrimSpace(g))
	if err != nil {
		return true // unparseable gen: don't reject a real device over it
	}
	return n >= 2
}

// shellyAddrFromEntry picks the first LAN IPv4 from the announcement and
// canonicalises it to the dial form (bare host, or host:port for a
// non-default port). The port comes from the SRV record; port 80/0 folds to
// the bare host. Reuses the manual-list LAN guard so discovery and the
// manual path share one address gate.
func shellyAddrFromEntry(e dnssd.Entry) (string, bool) {
	for _, a := range e.Addrs {
		if !a.Is4() {
			continue
		}
		ip := net.IP(a.AsSlice())
		// STRICTER than the manual list: discovery only auto-adopts RFC1918
		// private addresses, never loopback/link-local - a hostile mDNS
		// announcement must not make us dial our own localhost services.
		if ip == nil || !shellyDiscoverableIP(ip) {
			continue
		}
		host := a.String()
		port := e.Port
		if port == 0 || port == 80 {
			return host, true
		}
		norm, ok := normalizeShellyAddr(net.JoinHostPort(host, strconv.Itoa(int(port))))
		if !ok {
			return host, true // odd port failed the guard; fall back to the bare host
		}
		return norm, true
	}
	return "", false
}

// shellyMACFromEntry extracts a normalised MAC/device id. Shelly encodes it
// as the trailing hex of the instance label ("shellyplus1pm-a8032ab1c2d3")
// and often also in the "mac"/"id" TXT. Only a clean 12-hex-digit value is
// accepted; anything else yields "" (the device is still adopted, just
// keyed by address until a live probe learns its MAC).
func shellyMACFromEntry(e dnssd.Entry) string {
	if m := normalizeMAC(e.TXT["mac"]); m != "" {
		return m
	}
	if m := macFromShellyID(e.TXT["id"]); m != "" {
		return m
	}
	return macFromShellyID(e.InstanceLabel())
}

// shellyModelFromEntry derives a display model from the "app" TXT
// ("Plus1PM" -> "Shelly Plus1PM"); "" when absent (cosmetic only).
func shellyModelFromEntry(e dnssd.Entry) string {
	if app := strings.TrimSpace(e.TXT["app"]); app != "" {
		return "Shelly " + app
	}
	return ""
}

// macFromShellyID pulls the MAC out of a Shelly id like
// "shellyplus1pm-a8032ab1c2d3" (the part after the last '-'), normalising
// it. Returns "" when there is no clean 12-hex-digit tail.
func macFromShellyID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	if i := strings.LastIndexByte(id, '-'); i >= 0 {
		id = id[i+1:]
	}
	return normalizeMAC(id)
}

// normalizeMAC canonicalises a MAC/device-id tail to uppercase hex with no
// separators, requiring exactly 12 hex digits (a 48-bit MAC). Any other
// shape returns "" rather than a partial/garbage identity that could false-
// match on the ignore list.
func normalizeMAC(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(12)
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'A' && r <= 'F':
			b.WriteRune(r)
		case r >= 'a' && r <= 'f':
			b.WriteRune(r - 32) // to uppercase
		case r == ':' || r == '-' || r == '.':
			// separator - skip
		default:
			return "" // an unexpected character: not a MAC
		}
	}
	out := b.String()
	if len(out) != 12 {
		return ""
	}
	return out
}
