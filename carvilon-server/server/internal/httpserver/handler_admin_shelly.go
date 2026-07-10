// Saison 21 - Shelly Etappe 1: Shelly devices (Gen2+ local RPC, read
// only) as the Device Center's third real source next to UniFi and
// the local RPi readers. One flat row per physical device (category
// "switch", source "Shelly"); the slide-out panel lazily fetches the
// live switch-channel measurements (W / V / A / Hz / Wh) and inputs.
//
// Local-first: the only network the feature ever touches are the
// admin-configured LAN addresses - no Cloud.* calls, no discovery,
// no redirects. The addresses are validated to be LAN IPv4 targets
// at save time, the lazy detail endpoint only dials addresses that
// are part of the stored configuration, and neither the auth
// password nor an address ever reaches a log line (shellyapi errors
// come pre-redacted).
package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"carvilon.local/server/internal/platformconfig"
	"carvilon.local/server/internal/shellyapi"
	"carvilon.local/server/internal/shellycaps"
	"carvilon.local/server/internal/shellystore"
)

// shellyFleet is the immutable set of per-device clients. Swapped as
// one pointer (like the ua/protect clients) when the settings change.
type shellyFleet struct {
	clients []*shellyapi.Client
}

// SetShellyClients lets main (and the settings POST) swap the
// per-device Shelly clients after a config change. An empty set
// means "not configured".
func (s *Server) SetShellyClients(clients []*shellyapi.Client) {
	if len(clients) == 0 {
		s.shelly = nil
		return
	}
	s.shelly = &shellyFleet{clients: clients}
}

// shellyClientList returns the configured per-device clients (nil
// when unconfigured). The field is read exactly ONCE into a local:
// the settings POST swaps it (possibly to nil) while requests are in
// flight, and a check-then-deref double read would open a nil-panic
// window that the single-pointer ua/protect swaps do not have.
func (s *Server) shellyClientList() []*shellyapi.Client {
	if fleet := s.shelly; fleet != nil {
		return fleet.clients
	}
	return nil
}

// shellyEnabled ist der effektive "Shelly aktiv"-Schalter, gleiche
// Semantik wie uaEnabled/protectEnabled: explizites "1"/"0" gewinnt;
// fehlt der Wert, gilt an-wenn-Adressen-gesetzt.
func (s *Server) shellyEnabled(ctx context.Context) bool {
	if s.platformCfg == nil {
		return false
	}
	switch raw, _ := s.platformCfg.Get(ctx, platformconfig.KeyShellyEnabled); raw {
	case "1":
		return true
	case "0":
		return false
	default:
		// No explicit choice yet: on when at least one device is configured
		// (the set now lives in the shelly_devices table, not the legacy
		// address key). A store read error falls back to off.
		if s.shellystore == nil {
			return false
		}
		n, err := s.shellystore.CountActive(ctx)
		return err == nil && n > 0
	}
}

func (s *Server) shellyReady(ctx context.Context) bool {
	return s.shellyEnabled(ctx) && len(s.shellyClientList()) > 0
}

// shellyFriendlyError maps a shellyapi error to a fixed English
// message. Like uaFriendlyError it never embeds the underlying error
// text - the address/password must never reach the HTML or JSON.
func shellyFriendlyError(err error) string {
	if errors.Is(err, shellyapi.ErrUnauthorized) {
		return "Access denied - please check the Shelly auth password (401)."
	}
	return "Device unreachable or the response was invalid."
}

// maxShellyAddresses caps the configured list - the status poll fans
// out one HTTP request per device every few seconds, so an unbounded
// paste must not turn the poll into a flood.
const maxShellyAddresses = 32

// parseShellyAddresses turns the settings-form text into the
// normalised address list. Entries are separated by commas,
// semicolons or whitespace; a pasted URL form is reduced to its
// host[:port]. Every entry must be a LAN IPv4 (private, loopback or
// link-local - never a cloud metadata endpoint) with an optional
// port. Entries are CANONICALISED before deduping so equivalent
// spellings of one device collapse into one row: the host must be
// the plain dotted-quad form (no IPv4-mapped IPv6 text - the dial
// path could not use it), the port must be canonical decimal, and an
// empty or default-http port (":80", trailing ":") folds into the
// bare-host form. Order is preserved.
func parseShellyAddresses(raw string) ([]string, error) {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	})
	seen := map[string]bool{}
	var out []string
	for _, f := range fields {
		norm, ok := normalizeShellyAddr(f)
		if !ok {
			return nil, shellyAddrError(strings.TrimSpace(f))
		}
		if norm == "" { // an empty/whitespace field
			continue
		}
		if !seen[norm] {
			seen[norm] = true
			out = append(out, norm)
		}
	}
	if len(out) > maxShellyAddresses {
		return nil, errors.New("more than " + strconv.Itoa(maxShellyAddresses) + " device addresses - please trim the list")
	}
	return out, nil
}

// normalizeShellyAddr canonicalises one address entry to the dial form used
// everywhere (bare host, or host:port with a non-default port; the default
// http port ":80" and a trailing ":" fold into the bare form). The host
// must be a LAN IPv4 in plain dotted-quad spelling - no IPv4-mapped IPv6
// text (the dial path could not use it) and never an off-LAN or metadata
// address. Returns ("", true) for an empty/whitespace entry (skippable) and
// (_, false) for an invalid one. Shared by the settings parser, the
// store-backed client builder (defence in depth on a hand-edited row) and
// mDNS discovery, so one guard governs every path an address can take.
func normalizeShellyAddr(entry string) (string, bool) {
	addr := strings.TrimSpace(entry)
	for _, scheme := range []string{"http://", "https://"} {
		addr = strings.TrimPrefix(addr, scheme)
	}
	if i := strings.IndexByte(addr, '/'); i >= 0 {
		addr = addr[:i]
	}
	if addr == "" {
		return "", true
	}
	host, port := addr, ""
	if h, p, err := net.SplitHostPort(addr); err == nil {
		host, port = h, p
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return "", false
		}
		if n == 80 {
			port = "" // the default http port IS the bare form
		}
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.To4() == nil || ip.String() != host || !shellyLANIP(ip) {
		return "", false
	}
	if port != "" {
		return net.JoinHostPort(host, port), true
	}
	return host, true
}

func shellyAddrError(entry string) error {
	return errors.New("entry " + strconv.Quote(entry) + " is not a LAN IPv4 address (optionally with :port)")
}

// shellyLANIP mirrors the console LAN guard: private (RFC 1918),
// loopback or link-local targets only, and never the cloud
// instance-metadata endpoint - an admin form must not become an SSRF
// hop if this binary ever runs off the home LAN. This is the guard for
// the MANUAL admin list: an authenticated operator may deliberately pin a
// loopback/link-local target (e.g. a local dev stub).
func shellyLANIP(ip net.IP) bool {
	if ip.Equal(net.ParseIP("169.254.169.254")) {
		return false
	}
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
}

// shellyDiscoverableIP is the STRICTER guard for the UNTRUSTED mDNS
// discovery path: only genuine RFC 1918 private LAN addresses may be
// auto-adopted. Unlike shellyLANIP it rejects loopback and link-local, so
// a hostile announcement cannot make us auto-dial our own localhost
// services (127.0.0.0/8) or a link-local target (169.254.0.0/16, incl. the
// cloud metadata endpoint). ip.IsPrivate() is exactly 10/8, 172.16/12 and
// 192.168/16 - the addresses a home/building LAN actually uses.
func shellyDiscoverableIP(ip net.IP) bool {
	return ip.IsPrivate()
}

// BuildShellyClients constructs one client per stored address (the legacy
// comma-separated form, kept for the one-time seed). The stored value is
// trusted to have passed parseShellyAddresses, but a re-parse keeps a
// hand-edited database row from constructing clients for arbitrary targets.
func BuildShellyClients(addresses, password string) []*shellyapi.Client {
	parsed, err := parseShellyAddresses(addresses)
	if err != nil {
		return nil
	}
	return buildShellyClientsFromList(parsed, password)
}

// buildShellyClientsFromList constructs one client per address, re-checking
// each through the LAN guard (defence in depth: the addresses come from the
// shelly_devices table, which a hand-edit could poison). An address that
// fails the guard is dropped, not dialled.
func buildShellyClientsFromList(addresses []string, password string) []*shellyapi.Client {
	clients := make([]*shellyapi.Client, 0, len(addresses))
	for _, addr := range addresses {
		norm, ok := normalizeShellyAddr(addr)
		if !ok || norm == "" {
			continue
		}
		clients = append(clients, shellyapi.New(shellyapi.Options{Address: norm, Password: password}))
	}
	return clients
}

// rebuildShellyClients rebuilds the live client fleet from the active
// device set (manual + discovered) plus the shared digest password, and
// swaps it in. Called after any change to the set: a settings save, a
// manual removal, or an mDNS auto-adopt. A store/read error leaves the
// current fleet in place (never blanks a working set on a transient error).
func (s *Server) rebuildShellyClients(ctx context.Context) {
	if s.shellystore == nil {
		return
	}
	active, err := s.shellystore.ListActive(ctx)
	if err != nil {
		s.log.Warn("shelly: rebuild clients failed to list devices", "err", err)
		return
	}
	password, _ := s.platformCfg.GetSecret(ctx, platformconfig.KeyShellyPassword)
	addrs := make([]string, 0, len(active))
	for _, d := range active {
		addrs = append(addrs, d.Address)
	}
	s.SetShellyClients(buildShellyClientsFromList(addrs, password))
}

// SeedShellyManualFromLegacy imports the Etappe-1 comma-separated address
// list into the shelly_devices table exactly once (as manual devices),
// guarded by KeyShellyMigrated so a later-emptied set is never resurrected
// on the next start. A no-op once the flag is set or when there is nothing
// to import. Called by main before discovery starts.
func SeedShellyManualFromLegacy(ctx context.Context, store *shellystore.Store, cfg *platformconfig.Service, log *slog.Logger) {
	if store == nil || cfg == nil {
		return
	}
	if done, _ := cfg.Get(ctx, platformconfig.KeyShellyMigrated); done == "1" {
		return
	}
	// Set the migration flag BEFORE seeding. The legacy address key is never
	// rewritten, so if the flag were only set after a successful seed, a
	// failure here (or an admin who later removes a seeded address) could let
	// the next start re-import the legacy list and resurrect a removed
	// device. Flag-first trades that hazard for, at worst, a lost one-time
	// import on a rare config-write failure (the admin re-adds the IPs).
	if err := cfg.Set(ctx, platformconfig.KeyShellyMigrated, "1"); err != nil {
		log.Warn("shelly: set migration flag failed; deferring legacy seed to next start", "err", err)
		return
	}
	legacy, _ := cfg.Get(ctx, platformconfig.KeyShellyAddresses)
	if parsed, err := parseShellyAddresses(legacy); err == nil && len(parsed) > 0 {
		if err := store.ReplaceManual(ctx, parsed); err != nil {
			log.Warn("shelly: seed manual devices from legacy list failed (re-add them under /a/settings)", "err", err)
			return
		}
		log.Info("shelly: seeded manual devices from legacy address list", "count", len(parsed))
	}
}

// shellyProbe is one device's poll outcome: its client (for the
// address), the device info when reachable, and the redacted error
// otherwise. err != nil simply means "offline" - never a page error.
type shellyProbe struct {
	client *shellyapi.Client
	info   *shellyapi.DeviceInfo
	err    error
}

// probeShelly polls every configured device in parallel (one
// Shelly.GetDeviceInfo each - the method answers without auth, so it
// doubles as the reachability probe). The result keeps the configured
// order; an unreachable device is an offline entry, and one dead box
// never delays the page beyond the client timeout.
func (s *Server) probeShelly(ctx context.Context) []shellyProbe {
	clients := s.shellyClientList()
	if len(clients) == 0 {
		return nil
	}
	probes := make([]shellyProbe, len(clients))
	var wg sync.WaitGroup
	for i, c := range clients {
		wg.Add(1)
		go func(i int, c *shellyapi.Client) {
			defer wg.Done()
			info, err := c.GetDeviceInfo(ctx)
			probes[i] = shellyProbe{client: c, info: info, err: err}
		}(i, c)
	}
	wg.Wait()
	return probes
}

// makeShellyRow builds the flat Device Center row for one Shelly
// device (category "switch", source "Shelly"). The row identity is
// the CONFIGURED address - stable whether or not the device answers.
// An offline device keeps its row with the address as its name and
// "-" everywhere else; nothing is invented.
func makeShellyRow(p shellyProbe, info shellyRowInfo) uaRow {
	addr := p.client.Address()
	row := uaRow{
		ID:          addr,
		Kind:        "shelly",
		Category:    "switch",
		TypeLabel:   "Switch",
		Name:        addr,
		Source:      "shelly",
		SourceLabel: "Shelly",
		IP:          addr,
		Origin:      info.Origin,
		MQTTState:   info.MQTTState,
		ShellyID:    info.StoreID,
	}
	// Cockpit plumbing: the broker topic prefix (the provisioned account,
	// else the conventional "shelly-<mac>" the provisioner would assign)
	// and the capability-derived channel set. Model preference: the live
	// probe's answer, else the store's last-seen model - an offline
	// device still renders its channel skeleton.
	if user := info.MQTTUsername; user != "" {
		row.ShellyPrefix = "carvilon/" + user
	} else if info.MAC != "" {
		row.ShellyPrefix = "carvilon/shelly-" + strings.ToLower(info.MAC)
	}
	capModel := info.Model
	if p.info != nil && p.info.ModelLabel() != "" {
		capModel = p.info.ModelLabel()
	}
	if chans := shellycaps.Channels(capModel); len(chans) > 0 {
		type chJSON struct {
			ID    int  `json:"id"`
			Meter bool `json:"meter"`
		}
		list := make([]chJSON, 0, len(chans))
		for _, c := range chans {
			list = append(list, chJSON{ID: c.ID, Meter: c.Meter})
		}
		if raw, err := json.Marshal(list); err == nil {
			row.ChannelsJSON = string(raw)
		}
	}
	if p.err == nil {
		row.StatusState, row.StatusText = "online", "Online"
	} else {
		row.StatusState, row.StatusText = "offline", "Offline"
	}
	if p.info != nil {
		if n := p.info.DisplayName(); n != "" {
			row.Name = n
		}
		row.Model = p.info.ModelLabel()
		row.MAC = p.info.MACLabel()
		row.Firmware = p.info.FirmwareLabel()
	}

	det := []kvRow{
		{Key: "Type", Value: "Switch"},
		{Key: "Status", Value: row.StatusText},
		{Key: "Source", Value: row.SourceLabel},
	}
	det = appendKVDash(det, "Model", row.Model)
	det = appendKVDash(det, "IP address", row.IP)
	det = appendKVDash(det, "MAC", row.MAC)
	det = appendKVDash(det, "Firmware", row.Firmware)
	if p.info != nil {
		det = appendKV(det, "Authentication", p.info.AuthLabel())
		det = appendKVDash(det, "Device ID", p.info.IDLabel())
	} else {
		det = appendKVDash(det, "Device ID", "")
	}
	det = appendKV(det, "Origin", shellyOriginLabel(info.Origin))
	det = appendKV(det, "MQTT link", shellyMQTTStateLabel(info.MQTTState))
	row.Detail = det
	row.Search = strings.ToLower(strings.Join([]string{row.Name, row.Model, row.IP, row.MAC, row.TypeLabel, "shelly"}, " "))
	return row
}

// shellyOriginLabel renders the stored origin for the panel ("" for a row
// whose device is not in the store, e.g. a transient probe).
func shellyOriginLabel(origin string) string {
	switch origin {
	case shellystore.OriginManual:
		return "Manual (configured IP)"
	case shellystore.OriginDiscovered:
		return "Discovered (mDNS)"
	default:
		return ""
	}
}

// shellyMQTTStateLabel renders the MQTT provisioning state for the panel
// ("" hides the line for a device that was never provisioned).
func shellyMQTTStateLabel(state string) string {
	switch state {
	case shellystore.MQTTStateProvisioning:
		return "Provisioning…"
	case shellystore.MQTTStateLinked:
		return "Linked to broker"
	case shellystore.MQTTStateFailed:
		return "Provisioning failed - retry below"
	default:
		return ""
	}
}

// handleAdminUAShellyDetail lazily serves one device's live switch
// channels and inputs as panel sections when its row opens. The
// measurements are fetched fresh (Shelly.GetStatus + the channel
// names from Shelly.GetConfig) so the panel shows the moment's truth,
// not the page-load snapshot.
func (s *Server) handleAdminUAShellyDetail(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if !s.shellyReady(r.Context()) {
		writeUADetailError(w, "Shelly is not active or not configured.")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if !uaValidID(id) {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	// The id must match a CONFIGURED address: this endpoint must never
	// dial a caller-chosen target, only what the admin stored.
	var client *shellyapi.Client
	for _, c := range s.shellyClientList() {
		if c.Address() == id {
			client = c
			break
		}
	}
	if client == nil {
		writeUADetailError(w, "Not found.")
		return
	}

	st, err := client.GetStatus(r.Context())
	if err != nil {
		s.log.Warn("device center: shelly status failed", "err", err)
		writeUADetail(w, uaSection{Title: "Switch channels", Error: shellyFriendlyError(err)})
		return
	}
	// Names are cosmetic: a failed config read only drops the labels.
	cfg, cerr := client.GetConfig(r.Context())
	if cerr != nil {
		s.log.Warn("device center: shelly config failed", "err", cerr)
		cfg = nil
	}

	sections := make([]uaSection, 0, len(st.Switches)+1)
	for _, sw := range st.Switches {
		// 1-based titles match the O1..O4 print on the device; the
		// RPC's 0-based component ids stay an internal detail.
		title := "Switch " + strconv.Itoa(sw.ID+1)
		if name := cfg.SwitchName(sw.ID); name != "" {
			title += " · " + name
		}
		sec := uaSection{Title: title}
		sec.Rows = appendKVDash(sec.Rows, "State", sw.StateLabel())
		sec.Rows = appendKVDash(sec.Rows, "Power", sw.PowerLabel())
		sec.Rows = appendKVDash(sec.Rows, "Voltage", sw.VoltageLabel())
		sec.Rows = appendKVDash(sec.Rows, "Current", sw.CurrentLabel())
		sec.Rows = appendKVDash(sec.Rows, "Frequency", sw.FreqLabel())
		sec.Rows = appendKVDash(sec.Rows, "Energy", sw.EnergyLabel())
		sections = append(sections, sec)
	}
	if len(st.Inputs) > 0 {
		sec := uaSection{Title: "Inputs"}
		for _, in := range st.Inputs {
			key := "Input " + strconv.Itoa(in.ID+1)
			if name := cfg.InputName(in.ID); name != "" {
				key += " · " + name
			}
			sec.Rows = appendKVDash(sec.Rows, key, in.StateLabel())
		}
		sections = append(sections, sec)
	}
	writeUADetail(w, sections...)
}

// handleAdminShellySettingsPost speichert Adressliste + optionales
// Digest-Auth-Passwort + den "Shelly aktiv"-Schalter (eigenes Formular
// in /a/settings, Muster wie UA/Protect). Das Passwort landet
// AES-256-GCM-verschluesselt in platform_config und wird nie geloggt
// oder zurueckgerendert; danach werden die Clients sofort neu gebaut.
func (s *Server) handleAdminShellySettingsPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	rawAddrs := r.PostForm.Get("shelly_addresses")
	password := r.PostForm.Get("shelly_password")

	// The manual IP list is reconciled into the device table (an emptied
	// field removes the manual pins; discovered and ignored devices are
	// untouched), but only after validation - a bad entry keeps the stored
	// set untouched and flashes red.
	parsed, perr := parseShellyAddresses(rawAddrs)
	if perr != nil {
		data := s.buildSettingsData(r)
		data.Flash = "Device addresses: " + perr.Error()
		data.FlashType = "red"
		s.renderAdminPage(w, "settings", data)
		return
	}
	if s.shellystore != nil {
		if err := s.shellystore.ReplaceManual(r.Context(), parsed); err != nil {
			s.log.Error("save shelly manual addresses failed", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	if password != "" {
		if err := s.platformCfg.SetSecret(r.Context(), platformconfig.KeyShellyPassword, password); err != nil {
			s.log.Error("save shelly password failed", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}

	// Wie beim UA-/Protect-Schalter: die Checkbox sendet ihren Namen
	// nur wenn angehakt; wir schreiben immer explizit "1"/"0", damit
	// der Adressen-abhaengige Default danach nicht mehr greift.
	enabledVal := "0"
	if r.PostForm.Get("shelly_enabled") != "" {
		enabledVal = "1"
	}
	if err := s.platformCfg.Set(r.Context(), platformconfig.KeyShellyEnabled, enabledVal); err != nil {
		s.log.Error("save shelly_enabled failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.rebuildShellyClients(r.Context())

	data := s.buildSettingsData(r)
	data.Flash = "Saved."
	data.FlashType = "green"
	s.renderAdminPage(w, "settings", data)
}

// handleAdminShellyScan triggers one active mDNS scan from the settings
// page ("Scan now"). Discovery adopts on its own timeline; this only nudges
// the network. Redirects back so the async adoption surfaces on the next
// settings render / device-center poll.
func (s *Server) handleAdminShellyScan(w http.ResponseWriter, r *http.Request) {
	if s.shellyDisco != nil {
		s.shellyDisco.ScanNow()
	}
	http.Redirect(w, r, "/a/settings", http.StatusSeeOther)
}

// handleAdminShellyRelease removes one device from the ignore list (the
// "Ignored devices" view in settings), so a future announcement can adopt
// it again. Sticky removal is reversible - a mis-click is not permanent.
func (s *Server) handleAdminShellyRelease(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if s.shellystore == nil {
		http.Redirect(w, r, "/a/settings", http.StatusSeeOther)
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(r.PostForm.Get("id")), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := s.shellystore.ReleaseByID(r.Context(), id); err != nil && !errors.Is(err, shellystore.ErrNotFound) {
		s.log.Error("shelly: release ignored device failed", "err", err)
	}
	http.Redirect(w, r, "/a/settings", http.StatusSeeOther)
}

// shellyAutoAdopt is the effective "auto-activate discovered devices"
// setting. Default OFF (the approval gate is on): a discovered device waits
// as pending until approved. "1" restores Etappe-2 auto-adopt.
func (s *Server) shellyAutoAdopt(ctx context.Context) bool {
	if s.platformCfg == nil {
		return false
	}
	v, _ := s.platformCfg.Get(ctx, platformconfig.KeyShellyAutoAdopt)
	return v == "1"
}

// handleAdminShellyAutoAdopt saves the approval-gate toggle. It only changes
// the behaviour of NEW finds; existing pending devices are untouched (no
// surprise mass-activation when flipping the switch).
func (s *Server) handleAdminShellyAutoAdopt(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	val := "0"
	if r.PostForm.Get("shelly_auto_adopt") != "" {
		val = "1"
	}
	if err := s.platformCfg.Set(r.Context(), platformconfig.KeyShellyAutoAdopt, val); err != nil {
		s.log.Error("save shelly auto-adopt failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/a/settings", http.StatusSeeOther)
}

// handleAdminShellyKeepCloud saves the "keep Shelly cloud" opt-in used
// during provisioning. Default off disables the device cloud as hardening.
func (s *Server) handleAdminShellyKeepCloud(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	val := "0"
	if r.PostForm.Get("shelly_keep_cloud") != "" {
		val = "1"
	}
	if err := s.platformCfg.Set(r.Context(), platformconfig.KeyShellyKeepCloud, val); err != nil {
		s.log.Error("save shelly keep-cloud failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/a/settings", http.StatusSeeOther)
}

// handleAdminShellyApprove activates a pending (discovered) device: it joins
// the polled fleet. This is the one-click approval - the first time we ever
// talk to the device. Rebuilds the fleet so the poll picks it up.
func (s *Server) handleAdminShellyApprove(w http.ResponseWriter, r *http.Request) {
	s.shellyPendingAction(w, r, func(ctx context.Context, id int64) error {
		// The active cap (Etappe-1 limit) holds across approval too: a flood
		// is bounded via the pending cap, but manual approvals must not push
		// the polled fleet past maxShellyAddresses either.
		if err := s.shellystore.ApprovePending(ctx, id, maxShellyAddresses); err != nil {
			if errors.Is(err, shellystore.ErrAtCap) {
				s.log.Warn("shelly: approval rejected, active device cap reached",
					"cap", maxShellyAddresses)
			}
			return err
		}
		s.rebuildShellyClients(ctx)
		// Etappe 3, Phase 1: approval is when we first talk to the device -
		// provision it onto the MQTT broker (async; the row shows the state).
		s.startShellyProvision(id)
		return nil
	})
}

// handleAdminShellyReject sends a pending device to the sticky ignore list
// so discovery does not surface it again (releasable later like any ignored
// device). No fleet change - a rejected device was never polled.
func (s *Server) handleAdminShellyReject(w http.ResponseWriter, r *http.Request) {
	s.shellyPendingAction(w, r, func(ctx context.Context, id int64) error {
		return s.shellystore.RejectPending(ctx, id)
	})
}

// shellyPendingAction is the shared body of the approve/reject handlers:
// parse the id, run the action, redirect back. A missing pending row (double
// click, already handled) is not an error - it just redirects.
func (s *Server) shellyPendingAction(w http.ResponseWriter, r *http.Request, action func(context.Context, int64) error) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if s.shellystore == nil {
		http.Redirect(w, r, "/a/settings", http.StatusSeeOther)
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(r.PostForm.Get("id")), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	// ErrNotFound (double-click / already handled) and ErrAtCap (already
	// logged specifically by the approve handler) are expected outcomes, not
	// failures - just redirect back.
	if err := action(r.Context(), id); err != nil &&
		!errors.Is(err, shellystore.ErrNotFound) && !errors.Is(err, shellystore.ErrAtCap) {
		s.log.Error("shelly: pending action failed", "err", err)
	}
	http.Redirect(w, r, "/a/settings", http.StatusSeeOther)
}

// handleAdminUAShellyRemove is the sticky per-device removal from the Device
// Center panel: the device is forgotten from our active set and its identity
// (MAC when known, else the configured address) goes onto the ignore list so
// discovery does not re-adopt it. A CARVILON-side config action only - the
// device itself is never written to. The address must match a CONFIGURED
// active device (defence against a caller-chosen target). Redirects back to
// /a/devices with a stable flash code.
func (s *Server) handleAdminUAShellyRemove(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/a/devices?flash=shelly-err", http.StatusSeeOther)
		return
	}
	if s.shellystore == nil {
		http.Redirect(w, r, "/a/devices", http.StatusSeeOther)
		return
	}
	addr := strings.TrimSpace(r.PostForm.Get("address"))
	norm, ok := normalizeShellyAddr(addr)
	if !ok || norm == "" {
		http.Redirect(w, r, "/a/devices?flash=shelly-err", http.StatusSeeOther)
		return
	}
	// Learn the device's broker account (if provisioned) BEFORE removing, so
	// removal can also drop the credential - a forgotten device must not
	// leave a live broker login behind.
	var mqttUser string
	if active, lerr := s.shellystore.ListActive(r.Context()); lerr == nil {
		for _, d := range active {
			if d.Address == norm {
				mqttUser = d.MQTTUsername
				break
			}
		}
	}
	err := s.shellystore.RemoveByAddress(r.Context(), norm)
	switch {
	case errors.Is(err, shellystore.ErrNotFound):
		http.Redirect(w, r, "/a/devices?flash=shelly-notfd", http.StatusSeeOther)
		return
	case err != nil:
		s.log.Error("shelly: remove device failed", "err", err)
		http.Redirect(w, r, "/a/devices?flash=shelly-err", http.StatusSeeOther)
		return
	}
	if mqttUser != "" {
		s.deprovisionShellyCredential(mqttUser)
	}
	s.rebuildShellyClients(r.Context())
	http.Redirect(w, r, "/a/devices?flash=shelly-removed", http.StatusSeeOther)
}

// handleAdminUAShellyScan triggers an active mDNS scan from the Device
// Center toolbar. The live status poll + auto-reload surface any fresh
// adoption without a manual refresh.
func (s *Server) handleAdminUAShellyScan(w http.ResponseWriter, r *http.Request) {
	if s.shellyDisco != nil {
		s.shellyDisco.ScanNow()
	}
	http.Redirect(w, r, "/a/devices", http.StatusSeeOther)
}

// handleAdminUAShellyProvision (re)runs MQTT provisioning for one active
// device from the Device Center panel - the retry path when auto-
// provisioning on approval failed, and the way to provision a manually
// added device. Address must match a CONFIGURED active device.
func (s *Server) handleAdminUAShellyProvision(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || s.shellystore == nil {
		http.Redirect(w, r, "/a/devices?flash=shelly-err", http.StatusSeeOther)
		return
	}
	norm, ok := normalizeShellyAddr(r.PostForm.Get("address"))
	if !ok || norm == "" {
		http.Redirect(w, r, "/a/devices?flash=shelly-err", http.StatusSeeOther)
		return
	}
	if !s.shellyProvisionReady() {
		http.Redirect(w, r, "/a/devices?flash=shelly-noprov", http.StatusSeeOther)
		return
	}
	active, err := s.shellystore.ListActive(r.Context())
	if err != nil {
		http.Redirect(w, r, "/a/devices?flash=shelly-err", http.StatusSeeOther)
		return
	}
	for _, d := range active {
		if d.Address == norm {
			s.startShellyProvision(d.ID)
			http.Redirect(w, r, "/a/devices?flash=shelly-provisioning", http.StatusSeeOther)
			return
		}
	}
	http.Redirect(w, r, "/a/devices?flash=shelly-notfd", http.StatusSeeOther)
}

// buildShellySettingsBlock fills the settings block's Shelly section from
// the device table: the manual IP list (origin=manual, active) rendered
// back into the form, the count of auto-discovered devices, and the sticky
// ignore list. HasPassword/Enabled are set by the caller. Nil-store safe.
func (s *Server) buildShellySettingsBlock(ctx context.Context) shellySettingsBlock {
	var block shellySettingsBlock
	if s.shellystore == nil {
		return block
	}
	if manual, err := s.shellystore.ListManualActive(ctx); err == nil {
		addrs := make([]string, 0, len(manual))
		for _, d := range manual {
			addrs = append(addrs, d.Address)
		}
		block.Addresses = strings.Join(addrs, ", ")
	} else {
		s.log.Warn("shelly: list manual devices failed", "err", err)
	}
	if active, err := s.shellystore.ListActive(ctx); err == nil {
		for _, d := range active {
			if d.Origin == shellystore.OriginDiscovered {
				block.DiscoveredCount++
			}
		}
	}
	if pending, err := s.shellystore.ListPending(ctx); err == nil {
		for _, d := range pending {
			block.Pending = append(block.Pending, shellyPendingRow{
				ID: d.ID, MAC: d.MAC, Addr: d.Address,
			})
		}
	}
	if ignored, err := s.shellystore.ListIgnored(ctx); err == nil {
		for _, d := range ignored {
			label := d.MAC
			if label == "" {
				label = d.Address
			}
			block.Ignored = append(block.Ignored, shellyIgnoredRow{
				ID: d.ID, Label: label, MAC: d.MAC, Addr: d.Address,
			})
		}
	}
	return block
}

// handleAdminShellyStatus serves the settings block's "Connection"
// line: how many of the configured devices currently answer. Counts
// only - no addresses in the JSON. The probe runs on demand so the
// settings page itself renders instantly.
func (s *Server) handleAdminShellyStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	enabled := s.shellyEnabled(r.Context())
	clients := s.shellyClientList()
	out := map[string]any{
		"ok":      true,
		"enabled": enabled,
		"total":   len(clients),
	}
	if enabled && len(clients) > 0 {
		reachable := 0
		for _, p := range s.probeShelly(r.Context()) {
			if p.err == nil {
				reachable++
			}
		}
		out["reachable"] = reachable
	}
	_ = json.NewEncoder(w).Encode(out)
}
