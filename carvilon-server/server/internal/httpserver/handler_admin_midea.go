package httpserver

// Midea Climate Controller device family (Saison 21, Etappe 1) in the Device
// Center: local discovery -> approval gate -> adoption (cloud-primary /
// import-fallback credential fetch, verified by a local handshake, persisted
// encrypted) -> standard-profile cockpit (remote-like passthrough control +
// device sensor readout), plus per-device credential export. The control loop,
// analysis, history and setup wizard (the "advanced" profile) are gated off
// here and land in later etappen.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"carvilon.local/server/internal/mideaclimate"
	"carvilon.local/server/internal/mideamonitor"
	"carvilon.local/server/internal/mideastore"
)

// mideaRegion is the default NetHome-Plus cloud region for auto-adoption
// (briefing: WithRegion("DE") is the primary path). Overridable per-approval.
const mideaDefaultRegion = "DE"

// mideaReady reports whether the Midea device family is wired (store present).
// E1 has no separate on/off toggle: the source is available whenever the store
// is configured, and the approval gate keeps it safe out of the box.
func (s *Server) mideaReady() bool { return s.mideastore != nil }

// mideaLifecycleRows builds the Device-Center rows for the Midea source: the
// adopted (active) devices with their live status, plus the pending / ignored
// lifecycle rows. Nil-store safe.
func (s *Server) mideaLifecycleRows(ctx context.Context) (active, pending, ignored []uaRow) {
	if s.mideastore == nil {
		return nil, nil, nil
	}
	snap := map[string]mideamonitor.Readout{}
	if s.mideaMon != nil {
		for _, r := range s.mideaMon.Snapshot() {
			snap[r.ID] = r
		}
	}
	if act, err := s.mideastore.ListActive(ctx); err == nil {
		for _, d := range act {
			active = append(active, makeMideaRow(d, snap[d.ID]))
		}
	} else {
		s.log.Warn("midea: list active failed", "err", err)
	}
	if pend, err := s.mideastore.ListPending(ctx); err == nil {
		for _, d := range pend {
			pending = append(pending, makeMideaLifecycleRow(d, "midea-pending"))
		}
	}
	if ign, err := s.mideastore.ListIgnored(ctx); err == nil {
		for _, d := range ign {
			ignored = append(ignored, makeMideaLifecycleRow(d, "midea-ignored"))
		}
	}
	return active, pending, ignored
}

func mideaDisplayName(d mideastore.Device) string {
	if n := strings.TrimSpace(d.Name); n != "" {
		return n
	}
	return "Midea AC " + d.ID
}

// makeMideaRow renders one adopted Midea device as an active Device-Center row:
// the live readouts (mode, setpoint, fan, device return-air + outdoor sensor,
// "on device" badge) as Overview detail, and the current mode/fan/setpoint as
// the cockpit form defaults.
func makeMideaRow(d mideastore.Device, r mideamonitor.Readout) uaRow {
	online := r.Online
	statusText := "Offline"
	statusState := "offline"
	if online {
		statusText, statusState = "Online", "online"
	} else if r.Provisioning {
		statusText = "Connecting"
	}

	mode := r.Mode
	if mode == "" {
		mode = "cool"
	}
	fan := r.Fan
	if fan == "" {
		fan = "auto"
	}
	setpoint := "24.0"
	if r.Setpoint >= 17 && r.Setpoint <= 30 {
		setpoint = strconv.FormatFloat(r.Setpoint, 'f', 1, 64)
	}

	detail := []kvRow{
		{Key: "Control", Value: "on device"},
		{Key: "Mode", Value: mideaModeLabel(r.Mode, r.Power)},
		{Key: "Set temperature", Value: setpoint + " °C"},
		{Key: "Fan", Value: mideaFanLabel(fan)},
		{Key: "Return air (device sensor)", Value: tempOrDash(r.DeviceTempC, r.HasTemp)},
		{Key: "Outdoor", Value: tempOrDash(r.OutdoorC, r.HasOutdoor)},
		{Key: "Profile", Value: mideaProfileLabel(d.Profile)},
		{Key: "Protocol", Value: mideaProtocolLabel(d.ProtocolV3)},
	}
	if !online && r.LastErr != "" {
		detail = append(detail, kvRow{Key: "Last error", Value: r.LastErr})
	}

	name := mideaDisplayName(d)
	return uaRow{
		ID:            d.ID,
		Kind:          "midea",
		Category:      "midea-climate",
		TypeLabel:     "Climate controller",
		Name:          name,
		StatusState:   statusState,
		StatusText:    statusText,
		Source:        "midea",
		SourceLabel:   "Midea",
		Model:         "Midea Split AC",
		IP:            d.Address,
		Detail:        detail,
		Capabilities:  []string{"setpoint", "mode", "fan_mode", "sensor"},
		MideaMode:     mode,
		MideaFan:      fan,
		MideaSetpoint: setpoint,
		MideaProfile:  mideaProfileValue(d.Profile),
		Search:        strings.ToLower(name + " midea split ac " + d.Address + " " + d.ID),
	}
}

// makeMideaLifecycleRow renders a pending / ignored Midea device. Pending rows
// are pinned to the top group, ignored to the bottom, via the lifecycle
// pseudo-categories (shared with Shelly).
func makeMideaLifecycleRow(d mideastore.Device, kind string) uaRow {
	name := mideaDisplayName(d)
	category := "pending"
	life := "pending"
	statusText := "Pending approval"
	if kind == "midea-ignored" {
		category, life, statusText = "ignored", "ignored", "Ignored"
	}
	detail := []kvRow{
		{Key: "Source", Value: "Midea (local discovery)"},
		{Key: "IP", Value: d.Address},
		{Key: "Device id", Value: d.ID},
		{Key: "Protocol", Value: mideaProtocolLabel(d.ProtocolV3)},
	}
	return uaRow{
		ID:          d.ID,
		Kind:        kind,
		Category:    category,
		TypeLabel:   "Climate controller",
		Name:        name,
		StatusState: "unknown",
		StatusText:  statusText,
		Source:      "midea",
		SourceLabel: "Midea",
		Model:       "Midea Split AC",
		IP:          d.Address,
		Detail:      detail,
		Lifecycle:   life,
		Search:      strings.ToLower(name + " midea split ac " + d.Address + " " + d.ID),
	}
}

// --- labels ------------------------------------------------------------------

func mideaModeLabel(mode string, power bool) string {
	if !power || mode == "" || mode == "off" {
		return "Off"
	}
	switch mode {
	case "cool":
		return "Cool"
	case "heat":
		return "Heat"
	case "dry":
		return "Dry"
	case "fan_only":
		return "Fan only"
	case "auto":
		return "Auto"
	}
	return mode
}

func mideaFanLabel(fan string) string {
	switch fan {
	case "low":
		return "Low"
	case "mid":
		return "Mid"
	case "high":
		return "High"
	default:
		return "Auto"
	}
}

func mideaProfileValue(p string) string {
	if p == mideastore.ProfileAdvanced {
		return "advanced"
	}
	return "standard"
}

func mideaProfileLabel(p string) string {
	if p == mideastore.ProfileAdvanced {
		return "Advanced (server-side)"
	}
	return "Standard (on device)"
}

func mideaProtocolLabel(v3 bool) string {
	if v3 {
		return "V3 (token)"
	}
	return "V2"
}

func tempOrDash(c float64, has bool) string {
	if !has {
		return "—"
	}
	return strconv.FormatFloat(c, 'f', 1, 64) + " °C"
}

// --- handlers ----------------------------------------------------------------

func (s *Server) mideaRedirect(w http.ResponseWriter, r *http.Request, flash string) {
	http.Redirect(w, r, "/a/devices?flash="+flash, http.StatusSeeOther)
}

// handleAdminUAMideaScan runs a local UDP discovery (broadcast, or targeted at
// a host IP for VLAN/Windows robustness) and inserts every find as pending.
func (s *Server) handleAdminUAMideaScan(w http.ResponseWriter, r *http.Request) {
	if s.mideastore == nil {
		s.mideaRedirect(w, r, "midea-err")
		return
	}
	_ = r.ParseForm()
	host := strings.TrimSpace(r.PostForm.Get("host"))
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	found, err := mideaclimate.DiscoverLocal(ctx, host, 4*time.Second)
	if err != nil {
		s.log.Warn("midea: discovery failed", "host", host, "err", err)
		s.mideaRedirect(w, r, "midea-scan-err")
		return
	}
	added := 0
	for _, f := range found {
		det := mideastore.Detected{DeviceID: f.DeviceID, Address: f.IP, Name: f.Name, ProtocolV3: f.ProtocolV3}
		if host != "" {
			det.Origin = mideastore.OriginManual
		}
		if _, err := s.mideastore.InsertDiscovered(ctx, det); err != nil {
			s.log.Warn("midea: store discovered failed", "id", mideastore.IDFor(f.DeviceID), "err", err)
			continue
		}
		added++
	}
	if added == 0 {
		s.mideaRedirect(w, r, "midea-scan-none")
		return
	}
	s.mideaRedirect(w, r, "midea-scan-ok")
}

// handleAdminUAMideaApprove adopts a pending device. Credential fetch is
// cloud-primary (NetHome-Plus, region-selectable) with imported credentials as
// the fallback; Pair verifies them by a local 8370 handshake before we persist
// them encrypted, so a bad cloud key fails here and never yields a live broken
// device.
func (s *Server) handleAdminUAMideaApprove(w http.ResponseWriter, r *http.Request) {
	if s.mideastore == nil {
		s.mideaRedirect(w, r, "midea-err")
		return
	}
	_ = r.ParseForm()
	id := strings.TrimSpace(r.PostForm.Get("id"))
	region := strings.TrimSpace(r.PostForm.Get("region"))
	if region == "" {
		region = mideaDefaultRegion
	}
	importText := strings.TrimSpace(r.PostForm.Get("import"))

	dev, err := s.mideastore.Get(r.Context(), id)
	if err != nil || dev.State != mideastore.StatePending {
		s.mideaRedirect(w, r, "midea-notfd")
		return
	}
	discovered := mideaclimate.Discovered{
		IP: dev.Address, DeviceID: dev.DeviceID, Name: dev.Name, ProtocolV3: dev.ProtocolV3,
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// PRIMARY: cloud retrieval (region default DE). Pair fetches + verifies
	// locally.
	creds, perr := mideaclimate.Pair(ctx, discovered, mideaclimate.NewCloudRetriever(mideaclimate.WithRegion(region)))
	if perr != nil {
		s.log.Warn("midea: cloud pairing failed", "id", id, "region", region, "err", perr)
		// FALLBACK: imported credentials pasted by the operator. Use a FRESH
		// context: a slow-but-reachable cloud can consume the whole 30s budget
		// above, and the import path (a local-only handshake) must not inherit an
		// already-expired deadline - it is exactly the path that rescues a stuck
		// cloud (see pairing.go).
		if importText != "" {
			if ic, ierr := mideaclimate.ImportCredentialsFromExport(importText); ierr == nil {
				fbCtx, fbCancel := context.WithTimeout(r.Context(), 15*time.Second)
				creds, perr = mideaclimate.Pair(fbCtx, discovered,
					mideaclimate.NewImportedCredentials([]mideaclimate.Credentials{ic}))
				fbCancel()
				if perr != nil {
					s.log.Warn("midea: imported pairing failed", "id", id, "err", perr)
				}
			} else {
				s.log.Warn("midea: import parse failed", "id", id, "err", ierr)
				s.mideaRedirect(w, r, "midea-import-bad")
				return
			}
		}
	}
	if perr != nil {
		s.mideaRedirect(w, r, "midea-pair-err")
		return
	}
	if err := s.mideastore.Approve(r.Context(), id, creds.Token, creds.Key, mideastore.ProfileStandard); err != nil {
		s.log.Error("midea: approve persist failed", "id", id, "err", err)
		s.mideaRedirect(w, r, "midea-err")
		return
	}
	if s.mideaMon != nil {
		s.mideaMon.Refresh()
	}
	s.mideaRedirect(w, r, "midea-approved")
}

// handleAdminUAMideaReject / Release / Remove are the sticky-lifecycle actions.
func (s *Server) handleAdminUAMideaReject(w http.ResponseWriter, r *http.Request) {
	s.mideaLifecycleAction(w, r, "midea-ignored", func(ctx context.Context, id string) error {
		return s.mideastore.Reject(ctx, id)
	})
}

func (s *Server) handleAdminUAMideaRelease(w http.ResponseWriter, r *http.Request) {
	s.mideaLifecycleAction(w, r, "midea-released", func(ctx context.Context, id string) error {
		return s.mideastore.Release(ctx, id)
	})
}

func (s *Server) handleAdminUAMideaRemove(w http.ResponseWriter, r *http.Request) {
	s.mideaLifecycleAction(w, r, "midea-removed", func(ctx context.Context, id string) error {
		err := s.mideastore.Remove(ctx, id)
		if err == nil && s.mideaMon != nil {
			s.mideaMon.Refresh()
		}
		return err
	})
}

func (s *Server) mideaLifecycleAction(w http.ResponseWriter, r *http.Request, okFlash string, action func(context.Context, string) error) {
	if s.mideastore == nil {
		s.mideaRedirect(w, r, "midea-err")
		return
	}
	_ = r.ParseForm()
	id := strings.TrimSpace(r.PostForm.Get("id"))
	if id == "" {
		s.mideaRedirect(w, r, "midea-err")
		return
	}
	switch err := action(r.Context(), id); {
	case errors.Is(err, mideastore.ErrNotFound):
		s.mideaRedirect(w, r, "midea-notfd")
	case err != nil:
		s.log.Error("midea: lifecycle action failed", "id", id, "err", err)
		s.mideaRedirect(w, r, "midea-err")
	default:
		s.mideaRedirect(w, r, okFlash)
	}
}

// handleAdminUAMideaControl runs one standard-profile passthrough command
// (temperature / mode / fan) against a connected device.
func (s *Server) handleAdminUAMideaControl(w http.ResponseWriter, r *http.Request) {
	if s.mideaMon == nil || s.mideastore == nil {
		s.mideaRedirect(w, r, "midea-err")
		return
	}
	_ = r.ParseForm()
	id := strings.TrimSpace(r.PostForm.Get("id"))
	field := strings.TrimSpace(r.PostForm.Get("field"))
	value := strings.TrimSpace(r.PostForm.Get("value"))
	if id == "" {
		s.mideaRedirect(w, r, "midea-err")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	var err error
	switch field {
	case "temp":
		t, perr := strconv.ParseFloat(value, 64)
		if perr != nil || t < 17 || t > 30 {
			s.mideaRedirect(w, r, "midea-badval")
			return
		}
		err = s.mideaMon.SetTemperature(ctx, id, t)
	case "mode":
		if !mideaValidMode(value) {
			s.mideaRedirect(w, r, "midea-badval")
			return
		}
		err = s.mideaMon.SetMode(ctx, id, value)
	case "fan":
		if !mideaValidFan(value) {
			s.mideaRedirect(w, r, "midea-badval")
			return
		}
		err = s.mideaMon.SetFan(ctx, id, value)
	default:
		s.mideaRedirect(w, r, "midea-err")
		return
	}
	if err != nil {
		s.log.Warn("midea: control failed", "id", id, "field", field, "err", err)
		s.mideaRedirect(w, r, "midea-ctrl-err")
		return
	}
	s.mideaRedirect(w, r, "midea-sent")
}

// handleAdminUAMideaProfile switches the per-device profile. Advanced is gated
// off in E1 (the control loop / analysis / history land in later etappen), so
// only standard is accepted here; the toggle is present but advanced is inert.
func (s *Server) handleAdminUAMideaProfile(w http.ResponseWriter, r *http.Request) {
	if s.mideastore == nil {
		s.mideaRedirect(w, r, "midea-err")
		return
	}
	_ = r.ParseForm()
	id := strings.TrimSpace(r.PostForm.Get("id"))
	profile := strings.TrimSpace(r.PostForm.Get("profile"))
	if id == "" {
		s.mideaRedirect(w, r, "midea-err")
		return
	}
	if profile == mideastore.ProfileAdvanced {
		s.mideaRedirect(w, r, "midea-advanced-locked")
		return
	}
	if err := s.mideastore.SetProfile(r.Context(), id, mideastore.ProfileStandard); err != nil {
		s.log.Warn("midea: set profile failed", "id", id, "err", err)
		s.mideaRedirect(w, r, "midea-err")
		return
	}
	s.mideaRedirect(w, r, "midea-profile")
}

// handleAdminUAMideaExport serves the device's permanent V3 credentials as a
// downloadable text file so the operator can keep them against the day Midea
// shuts the cloud token API. This is the user exporting their own device keys.
func (s *Server) handleAdminUAMideaExport(w http.ResponseWriter, r *http.Request) {
	if s.mideastore == nil {
		http.Error(w, "not available", http.StatusServiceUnavailable)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	dev, err := s.mideastore.Get(r.Context(), id)
	if err != nil || dev.State != mideastore.StateActive {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	token, key, err := s.mideastore.Credential(r.Context(), id)
	if err != nil {
		s.log.Warn("midea: export creds failed", "id", id, "err", err)
		http.Error(w, "credentials unavailable", http.StatusNotFound)
		return
	}
	text := mideaclimate.ExportCredentials(mideaclimate.Credentials{
		IP: dev.Address, DeviceID: dev.DeviceID, Token: token, Key: key,
	})
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="midea-%s-credentials.txt"`, id))
	_, _ = w.Write([]byte(text))
}

func mideaValidMode(v string) bool {
	switch v {
	case "off", "cool", "heat", "dry", "fan_only", "auto":
		return true
	}
	return false
}

func mideaValidFan(v string) bool {
	switch v {
	case "auto", "low", "mid", "high":
		return true
	}
	return false
}
