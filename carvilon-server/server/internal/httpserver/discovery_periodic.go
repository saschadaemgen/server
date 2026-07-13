package httpserver

// Unified, automatic device discovery.
//
// One "Scan network" action (and an automatic periodic background sweep) drive
// the SAME set of registered discovery sources at once, instead of a separate
// button per system: the Shelly mDNS re-scan (shellyDisco.ScanNow), the Shelly
// active subnet sweep (shellyScan.Start) and the Midea UDP broadcast
// (mideaclimate.DiscoverLocal). Every source is LAN-guarded and deduped by its
// own store, so a periodic run never re-adopts or double-lists a device, and a
// newly powered device shows up on its own without a manual scan.

import (
	"context"
	"net"
	"net/http"
	"time"

	"carvilon.local/server/internal/mideaclimate"
	"carvilon.local/server/internal/mideastore"
)

const (
	// discoverySweepInterval is how often the automatic background sweep runs
	// the sources that were on-demand-only before (the Shelly active subnet
	// scan + the Midea broadcast). Shelly mDNS already has its own 4-minute
	// backstop. Lightweight: identify/broadcast only, coalesced per source.
	discoverySweepInterval = 3 * time.Minute
	// discoveryWarmupDelay lets the network settle after boot before the first
	// automatic sweep (the manual button is available immediately).
	discoveryWarmupDelay = 20 * time.Second
	// mideaDiscoverTimeout bounds one Midea discovery run; mideaDiscoverListen
	// is how long it waits for UDP responses.
	mideaDiscoverTimeout = 6 * time.Second
	mideaDiscoverListen  = 4 * time.Second
)

// RunPeriodicDiscovery runs the automatic background discovery sweep until ctx
// is cancelled. main launches it; a no-op when no discovery source is wired.
func (s *Server) RunPeriodicDiscovery(ctx context.Context) {
	if s.shellyScan == nil && s.shellyDisco == nil && s.mideastore == nil {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(discoveryWarmupDelay):
	}
	ticker := time.NewTicker(discoverySweepInterval)
	defer ticker.Stop()
	for {
		s.runAllDiscovery(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runAllDiscovery triggers every registered discovery source once. Shared by the
// manual "Scan network" button and the periodic sweep. Each source is coalesced
// / bounded internally, so concurrent triggers never stack.
func (s *Server) runAllDiscovery(ctx context.Context) {
	if s.shellyDisco != nil {
		s.shellyDisco.ScanNow()
	}
	if s.shellyScan != nil {
		if subnets, err := s.shellyScan.subnets(); err != nil {
			s.log.Warn("discovery: enumerate subnets failed", "err", err)
		} else if len(subnets) > 0 {
			s.shellyScan.Start(ctx, subnets, scanPortDefault)
		}
	}
	if s.mideastore != nil {
		func() {
			s.mideaScanActive.Add(1)
			defer s.mideaScanActive.Add(-1)
			s.mideaDiscover(ctx, "")
		}()
	}
}

// mideaDiscover runs one Midea UDP discovery (broadcast when host is empty, or
// targeted at host), LAN-guards every responder, and upserts finds as pending
// (deduped by the store). Returns how many were stored. Callers own the
// mideaScanActive counter (they bump it synchronously so the unified status
// reports "running" from the very first poll).
func (s *Server) mideaDiscover(ctx context.Context, host string) int {
	if s.mideastore == nil {
		return 0
	}
	dctx, cancel := context.WithTimeout(ctx, mideaDiscoverTimeout)
	defer cancel()
	found, err := mideaclimate.DiscoverLocal(dctx, host, mideaDiscoverListen)
	if err != nil {
		s.log.Warn("midea: discovery failed", "host", host, "err", err)
		return 0
	}
	added := 0
	for _, f := range found {
		if !mideaDiscoverableIP(f.IP) {
			s.log.Warn("midea: discovery skipped non-LAN responder", "ip", f.IP)
			continue
		}
		det := mideastore.Detected{DeviceID: f.DeviceID, Address: f.IP, Name: f.Name, ProtocolV3: f.ProtocolV3}
		if host != "" {
			det.Origin = mideastore.OriginManual
		}
		if _, err := s.mideastore.InsertDiscovered(dctx, det); err != nil {
			s.log.Warn("midea: store discovered failed", "id", mideastore.IDFor(f.DeviceID), "err", err)
			continue
		}
		added++
	}
	return added
}

// mideaDiscoverableIP is the LAN-guard for Midea discovery responders: private
// or loopback (dev stub) only, so a spoofed off-LAN responder is never stored
// and link-local / IMDS (169.254/16) is excluded.
func mideaDiscoverableIP(ip string) bool {
	p := net.ParseIP(ip)
	if p == nil {
		return false
	}
	return p.IsPrivate() || p.IsLoopback()
}

// scanRunning reports whether any discovery source is mid-sweep, for the unified
// status endpoint.
func (s *Server) scanRunning() bool {
	if s.mideaScanActive.Load() > 0 {
		return true
	}
	if s.shellyScan != nil && s.shellyScan.Snapshot().Running {
		return true
	}
	return false
}

// handleAdminDevicesScan is the single manual "Scan network" action: it triggers
// EVERY registered discovery source at once and returns immediately; the
// frontend polls /a/devices/scan/status for progress, then reloads to show new
// Pending rows.
func (s *Server) handleAdminDevicesScan(w http.ResponseWriter, r *http.Request) {
	if s.shellyScan == nil && s.shellyDisco == nil && s.mideastore == nil {
		designerJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "No discovery source is available."})
		return
	}
	if s.shellyDisco != nil {
		s.shellyDisco.ScanNow()
	}
	if s.shellyScan != nil {
		if subnets, err := s.shellyScan.subnets(); err != nil {
			s.log.Warn("discovery: enumerate subnets failed", "err", err)
		} else if len(subnets) > 0 {
			s.shellyScan.Start(r.Context(), subnets, scanPortDefault)
		}
	}
	if s.mideastore != nil {
		// DiscoverLocal blocks a few seconds; run it detached so the POST
		// returns at once. Bump the scan-active counter SYNCHRONOUSLY here (not
		// inside the goroutine) so the very first status poll already sees it
		// running - otherwise the poll could report "complete" and reload before
		// the goroutine is even scheduled.
		s.mideaScanActive.Add(1)
		go func(ctx context.Context) {
			defer s.mideaScanActive.Add(-1)
			s.mideaDiscover(ctx, "")
		}(context.WithoutCancel(r.Context()))
	}
	designerJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true, "running": true})
}

// handleAdminDevicesScanStatus returns the unified scan progress (the Shelly
// active-sweep counters plus whether the Midea broadcast is still running).
func (s *Server) handleAdminDevicesScanStatus(w http.ResponseWriter, r *http.Request) {
	var probed, total, found, newCount int
	if s.shellyScan != nil {
		snap := s.shellyScan.Snapshot()
		probed, total, found, newCount = snap.Probed, snap.Total, snap.Found, snap.New
	}
	running := s.scanRunning()
	msg := ""
	if !running {
		msg = "Scan complete."
	}
	designerJSON(w, http.StatusOK, map[string]any{
		"ok": true, "running": running,
		"probed": probed, "total": total, "found": found, "new": newCount,
		"message": msg,
	})
}
