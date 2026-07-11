// Active subnet scan for Shelly discovery. A device with
// "discoverable": false (the real RGBW2 ships that way) never announces
// over mDNS, so passive discovery can never find it and manual-IP entry
// is the only fallback - a poor experience. This is the Shelly app's
// answer: an on-demand sweep of the edge's own LAN that probes each host
// with the harmless, unauthenticated GET /shelly identify endpoint,
// classifies the responders, and feeds new ones into the SAME approval
// gate as mDNS finds (tagged "found by scan", never auto-adopted).
//
// Safety rails (hard):
//   - the sweep never leaves the edge's own RFC1918 subnet(s): the target
//     list is derived from the /24 around each own private interface
//     address, and every target is re-checked against those subnets
//     before it is dialled (own24Subnets + subnetContains, both pure and
//     unit-tested);
//   - only /shelly is probed - no auth, no /settings, no writes;
//   - bounded parallelism + a short per-host timeout, so the whole sweep
//     finishes in seconds and cannot flood the network;
//   - on-demand only (no background/periodic scanning in this tranche);
//   - the ignore list and the MAC-dedupe are honoured by the shared
//     store.Adopt path, exactly as for mDNS.
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

	"carvilon.local/server/internal/shelly1api"
	"carvilon.local/server/internal/shellycaps"
	"carvilon.local/server/internal/shellystore"
)

const (
	// scanProbeTimeout bounds one host's GET /shelly. A live Shelly answers
	// in a few ms; a dead host must fail fast so the sweep stays quick.
	scanProbeTimeout = 1200 * time.Millisecond
	// scanWorkers caps concurrent probes - a /24 (254 hosts) drains in a
	// handful of batches, so the whole sweep is a few seconds.
	scanWorkers = 64
	// scanPortDefault is the Shelly HTTP port (all Gen1/Gen2 devices).
	scanPortDefault = 80
	// scanMaxSubnets / scanMaxTargets bound a pathological interface set so
	// a mis-configured host cannot turn a scan into a huge sweep.
	scanMaxSubnets = 8
	scanMaxTargets = 4096
)

// scanProgress is a snapshot of a running (or finished) scan for the UI.
type scanProgress struct {
	Running bool   `json:"running"`
	Total   int    `json:"total"`
	Probed  int    `json:"probed"`
	Found   int    `json:"found"` // responders that classified as an in-scope Shelly
	New     int    `json:"new"`   // of those, newly recorded as pending
	Done    bool   `json:"done"`  // a scan has completed at least once this process
	Message string `json:"message,omitempty"`
}

// shellyScanner runs on-demand active sweeps. probe and subnets are
// injectable so the whole pipeline can be exercised deterministically in
// tests without touching a real network.
type shellyScanner struct {
	store   *shellystore.Store
	log     *slog.Logger
	enabled func(context.Context) bool
	// probe performs the harmless GET /shelly identify against a dial
	// address ("ip" or "ip:port"); ok is false for no/!Shelly answer.
	probe func(ctx context.Context, addr string) (*shelly1api.Identity, bool)
	// subnets returns the edge's own RFC1918 /24 subnets (production wires
	// enumerateOwnSubnets); a nil/empty result means "nothing to scan".
	subnets func() ([]netip.Prefix, error)
	limit   int
	workers int
	timeout time.Duration

	running atomic.Bool

	mu   sync.Mutex
	prog scanProgress
}

// newShellyScanner wires a scanner with production defaults.
func newShellyScanner(store *shellystore.Store, log *slog.Logger, enabled func(context.Context) bool,
	probe func(context.Context, string) (*shelly1api.Identity, bool),
	subnets func() ([]netip.Prefix, error)) *shellyScanner {
	if log == nil {
		log = slog.Default()
	}
	return &shellyScanner{
		store: store, log: log, enabled: enabled, probe: probe, subnets: subnets,
		limit: maxShellyAddresses, workers: scanWorkers, timeout: scanProbeTimeout,
	}
}

// Snapshot returns the current progress (safe for concurrent readers).
func (sc *shellyScanner) Snapshot() scanProgress {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.prog
}

// Start kicks off a sweep of the given /24 subnets on the given port. It
// coalesces: a second Start while one runs is a no-op returning false, so
// a burst of button clicks cannot pile up sweeps. subnets MUST already be
// own RFC1918 /24s (the caller derives them); each target is still
// re-guarded against them before dialling.
func (sc *shellyScanner) Start(ctx context.Context, subnets []netip.Prefix, port int) bool {
	if sc == nil || sc.store == nil {
		return false
	}
	// Defence in depth: the scanner itself refuses to run while the Shelly
	// integration is off, independent of the handler's own gate.
	if sc.enabled != nil && !sc.enabled(ctx) {
		return false
	}
	if !sc.running.CompareAndSwap(false, true) {
		return false // a scan is already in flight
	}
	if port <= 0 || port > 65535 {
		port = scanPortDefault
	}
	targets := planScanTargets(subnets)
	sc.mu.Lock()
	sc.prog = scanProgress{Running: true, Total: len(targets)}
	sc.mu.Unlock()

	go func() {
		defer sc.running.Store(false)
		bg := context.WithoutCancel(ctx) // survive the triggering request
		sc.run(bg, subnets, targets, port)
	}()
	return true
}

// run performs the sweep: bounded workers probe each target's /shelly,
// classify the responders, and fold new ones into the approval gate. It
// never auto-adopts (the scan always records pending, regardless of the
// mDNS auto-adopt setting - a scan is an explicit find, but adoption
// stays the operator's).
func (sc *shellyScanner) run(ctx context.Context, subnets []netip.Prefix, targets []netip.Addr, port int) {
	var probed, found, newCount atomic.Int64

	sem := make(chan struct{}, sc.workers)
	var wg sync.WaitGroup
	for _, target := range targets {
		// Defence in depth: never dial a target that is not inside the
		// derived own subnets, whatever produced the list.
		if !subnetContains(subnets, target) {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(ip netip.Addr) {
			defer wg.Done()
			defer func() {
				<-sem
				n := probed.Add(1)
				sc.setProbed(int(n))
			}()
			pctx, cancel := context.WithTimeout(ctx, sc.timeout)
			defer cancel()
			ident, ok := sc.probe(pctx, dialAddr(ip, port))
			if !ok {
				return
			}
			det, ok := detectedFromScan(ident, dialAddr(ip, port))
			if !ok {
				return // answered /shelly but not an in-scope Shelly
			}
			found.Add(1)
			res, err := sc.store.Adopt(ctx, det, sc.limit, false /* never auto-adopt */)
			if err != nil {
				sc.log.Warn("shelly scan: adopt failed", "component", "shelly-scan", "err", err)
				return
			}
			if res == shellystore.AdoptedPending {
				newCount.Add(1)
			}
		}(target)
	}
	wg.Wait()

	sc.mu.Lock()
	sc.prog = scanProgress{
		Running: false, Done: true,
		Total: len(targets), Probed: int(probed.Load()),
		Found: int(found.Load()), New: int(newCount.Load()),
	}
	if len(targets) == 0 {
		sc.prog.Message = "No local network to scan (no private LAN interface)."
	}
	sc.mu.Unlock()
	sc.log.Info("shelly network scan finished", "component", "shelly-scan",
		"probed", probed.Load(), "found", found.Load(), "new", newCount.Load())
}

func (sc *shellyScanner) setProbed(n int) {
	sc.mu.Lock()
	if n > sc.prog.Probed {
		sc.prog.Probed = n
	}
	sc.mu.Unlock()
}

// detectedFromScan classifies a /shelly identify answer into a store
// Detected, or ok=false when it is not an in-scope Shelly. It requires a
// MAC (a real Shelly always reports one on /shelly - it also makes the
// MAC-dedupe the briefing asks for actually work), a resolved generation,
// and - for Gen1 - a supported type code (mirroring the mDNS allowlist
// scope, so the scan never surfaces a device class the cockpit cannot
// handle). Origin is tagged scanned so the pending row shows how it was
// found.
func detectedFromScan(ident *shelly1api.Identity, addr string) (shellystore.Detected, bool) {
	gen := ident.Generation()
	if gen <= 0 {
		return shellystore.Detected{}, false
	}
	mac := normalizeMAC(ident.MACLabel())
	if mac == "" {
		return shellystore.Detected{}, false
	}
	if gen == shellystore.Gen1 && !shellycaps.IsGen1Type(ident.TypeLabel()) {
		return shellystore.Detected{}, false
	}
	// A non-empty, human display name (never blanks an existing mDNS name
	// on a re-find): Gen1 stores the raw type code as Model, so its human
	// label comes from the caps table; Gen2 already reports a human model.
	name := shellyIdentModel(ident, gen)
	if gen == shellystore.Gen1 {
		name = shellycaps.Gen1ModelLabel(ident.TypeLabel())
	}
	return shellystore.Detected{
		MAC:     mac,
		Address: addr,
		Name:    name,
		Model:   shellyIdentModel(ident, gen),
		Gen:     gen,
		Origin:  shellystore.OriginScanned,
	}, true
}

// dialAddr renders a target IP + port as the dial form: bare host for the
// default HTTP port (the canonical form everywhere), host:port otherwise.
func dialAddr(ip netip.Addr, port int) string {
	if port == scanPortDefault {
		return ip.String()
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(port))
}

// ---- pure subnet planning + guard (unit-tested) ----

// own24Subnets reduces a list of interface prefixes to the distinct
// RFC1918 /24 subnets to sweep: it keeps only genuine private IPv4
// unicast addresses (never loopback, link-local, or public), masks each
// to /24 (the briefing caps the sweep at /24 even when the interface mask
// is wider), and dedupes. Bounded to scanMaxSubnets so a host with many
// aliased interfaces cannot explode the sweep.
func own24Subnets(prefixes []netip.Prefix) []netip.Prefix {
	seen := map[netip.Prefix]bool{}
	var out []netip.Prefix
	for _, p := range prefixes {
		ip := p.Addr()
		if !ip.Is4() || !ip.IsPrivate() {
			continue // only RFC1918 IPv4 - the LAN guard, at the source
		}
		net24, err := ip.Prefix(24)
		if err != nil {
			continue
		}
		net24 = net24.Masked()
		if seen[net24] {
			continue
		}
		seen[net24] = true
		out = append(out, net24)
		if len(out) >= scanMaxSubnets {
			break
		}
	}
	return out
}

// planScanTargets expands the /24 subnets to their host addresses
// (.1 .. .254, skipping the network and broadcast addresses), deduped and
// bounded to scanMaxTargets.
func planScanTargets(subnets []netip.Prefix) []netip.Addr {
	seen := map[netip.Addr]bool{}
	var out []netip.Addr
	for _, sub := range subnets {
		if sub.Bits() != 24 || !sub.Addr().Is4() {
			continue
		}
		base := sub.Masked().Addr().As4()
		for host := 1; host <= 254; host++ {
			a := base
			a[3] = byte(host)
			addr := netip.AddrFrom4(a)
			if seen[addr] {
				continue
			}
			seen[addr] = true
			out = append(out, addr)
			if len(out) >= scanMaxTargets {
				return out
			}
		}
	}
	return out
}

// subnetContains reports whether addr falls inside any of the subnets -
// the last-line guard applied to every target before it is dialled.
func subnetContains(subnets []netip.Prefix, addr netip.Addr) bool {
	for _, sub := range subnets {
		if sub.Contains(addr) {
			return true
		}
	}
	return false
}

// shellyIdentifyProbe is the production prober: one harmless GET /shelly
// (unauthenticated by contract - no password is ever attached to the
// identify endpoint) against a dial address. It is the ONLY device
// contact the scan makes.
func (s *Server) shellyIdentifyProbe(ctx context.Context, addr string) (*shelly1api.Identity, bool) {
	cl := shelly1api.New(shelly1api.Options{Address: addr, Timeout: scanProbeTimeout})
	ident, err := cl.GetIdentity(ctx)
	if err != nil {
		return nil, false
	}
	return ident, true
}

// handleAdminShellyScanNetwork starts an active subnet sweep and returns
// immediately (the UI polls the status endpoint for progress). Route:
// POST /a/settings/shelly/scan-network.
//
// Production sweeps the edge's own RFC1918 /24 subnets on port 80. In
// DevMode ONLY, optional "subnet" (a single RFC1918-or-loopback CIDR,
// masked to /24) and "port" form params override the target - so the
// feature can be exercised end to end against a local stub without a
// real device on the LAN (the same DevMode-only precedent as the
// dev-announce endpoint). The override never widens production: outside
// DevMode the params are ignored and enumeration + port 80 are used.
func (s *Server) handleAdminShellyScanNetwork(w http.ResponseWriter, r *http.Request) {
	if s.shellyScan == nil {
		http.Error(w, "scan not available", http.StatusServiceUnavailable)
		return
	}
	if !s.shellyEnabled(r.Context()) {
		designerJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "Shelly integration is off."})
		return
	}
	port := scanPortDefault
	var subnets []netip.Prefix
	if s.cfg.DevMode {
		_ = r.ParseForm()
		if sub, ok := devScanSubnet(r.PostFormValue("subnet")); ok {
			subnets = []netip.Prefix{sub}
		}
		if p, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("port"))); err == nil && p > 0 && p <= 65535 {
			port = p
		}
	}
	if len(subnets) == 0 {
		nets, err := s.shellyScan.subnets()
		if err != nil {
			s.log.Warn("shelly scan: enumerate subnets failed", "component", "shelly-scan", "err", err)
		}
		subnets = nets
	}
	started := s.shellyScan.Start(r.Context(), subnets, port)
	designerJSON(w, http.StatusOK, map[string]any{"ok": true, "started": started, "running": !started})
}

// handleAdminShellyScanNetworkStatus returns the live scan progress.
// Route: GET /a/settings/shelly/scan-network/status.
func (s *Server) handleAdminShellyScanNetworkStatus(w http.ResponseWriter, r *http.Request) {
	if s.shellyScan == nil {
		designerJSON(w, http.StatusOK, scanProgress{})
		return
	}
	designerJSON(w, http.StatusOK, s.shellyScan.Snapshot())
}

// devScanSubnet parses a DevMode-only scan target CIDR into a /24. It
// accepts RFC1918 OR loopback (a dev stub commonly binds 127.0.0.1),
// always masks to /24, and rejects everything else - so even the dev
// override cannot aim the sweep off the local machine.
func devScanSubnet(cidr string) (netip.Prefix, bool) {
	cidr = strings.TrimSpace(cidr)
	if cidr == "" {
		return netip.Prefix{}, false
	}
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return netip.Prefix{}, false
	}
	ip := p.Addr()
	if !ip.Is4() || !(ip.IsPrivate() || ip.IsLoopback()) {
		return netip.Prefix{}, false
	}
	net24, err := ip.Prefix(24)
	if err != nil {
		return netip.Prefix{}, false
	}
	return net24.Masked(), true
}

// enumerateOwnSubnets lists the edge host's own network interfaces and
// returns their RFC1918 /24 subnets - the production subnet source. A
// listing error yields no subnets (the scan then reports nothing to do)
// rather than a failure.
func enumerateOwnSubnets() ([]netip.Prefix, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var prefixes []netip.Prefix
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipnet.IP)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			if !ip.Is4() {
				continue
			}
			ones, _ := ipnet.Mask.Size()
			if p, err := ip.Prefix(ones); err == nil {
				prefixes = append(prefixes, p)
			}
		}
	}
	return own24Subnets(prefixes), nil
}
