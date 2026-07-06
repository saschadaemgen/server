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
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"carvilon.local/server/internal/platformconfig"
	"carvilon.local/server/internal/shellyapi"
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
		addrs, _ := s.platformCfg.Get(ctx, platformconfig.KeyShellyAddresses)
		return strings.TrimSpace(addrs) != ""
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
		addr := strings.TrimSpace(f)
		for _, scheme := range []string{"http://", "https://"} {
			addr = strings.TrimPrefix(addr, scheme)
		}
		if i := strings.IndexByte(addr, '/'); i >= 0 {
			addr = addr[:i]
		}
		if addr == "" {
			continue
		}
		host, port := addr, ""
		if h, p, err := net.SplitHostPort(addr); err == nil {
			host, port = h, p
		}
		if port != "" {
			n, err := strconv.Atoi(port)
			if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
				return nil, shellyAddrError(addr)
			}
			if n == 80 {
				port = "" // the default http port IS the bare form
			}
		}
		ip := net.ParseIP(host)
		if ip == nil || ip.To4() == nil || ip.String() != host || !shellyLANIP(ip) {
			return nil, shellyAddrError(addr)
		}
		norm := host
		if port != "" {
			norm = net.JoinHostPort(host, port)
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

func shellyAddrError(entry string) error {
	return errors.New("entry " + strconv.Quote(entry) + " is not a LAN IPv4 address (optionally with :port)")
}

// shellyLANIP mirrors the console LAN guard: private (RFC 1918),
// loopback or link-local targets only, and never the cloud
// instance-metadata endpoint - an admin form must not become an SSRF
// hop if this binary ever runs off the home LAN.
func shellyLANIP(ip net.IP) bool {
	if ip.Equal(net.ParseIP("169.254.169.254")) {
		return false
	}
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
}

// BuildShellyClients constructs one client per stored address (main
// uses it at startup, the settings POST after a save). The stored
// value is trusted to have passed parseShellyAddresses, but a
// re-parse keeps a hand-edited database row from constructing clients
// for arbitrary targets.
func BuildShellyClients(addresses, password string) []*shellyapi.Client {
	parsed, err := parseShellyAddresses(addresses)
	if err != nil {
		return nil
	}
	clients := make([]*shellyapi.Client, 0, len(parsed))
	for _, addr := range parsed {
		clients = append(clients, shellyapi.New(shellyapi.Options{Address: addr, Password: password}))
	}
	return clients
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
func makeShellyRow(p shellyProbe) uaRow {
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
	row.Detail = det
	row.Search = strings.ToLower(strings.Join([]string{row.Name, row.Model, row.IP, row.MAC, row.TypeLabel, "shelly"}, " "))
	return row
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

	// The address list is always written (an emptied field means
	// "no devices"), but only after validation - a bad entry keeps
	// the stored list untouched and flashes red.
	parsed, perr := parseShellyAddresses(rawAddrs)
	if perr != nil {
		data := s.buildSettingsData(r)
		data.Flash = "Device addresses: " + perr.Error()
		data.FlashType = "red"
		s.renderAdminPage(w, "settings", data)
		return
	}
	if err := s.platformCfg.Set(r.Context(), platformconfig.KeyShellyAddresses, strings.Join(parsed, ", ")); err != nil {
		s.log.Error("save shelly addresses failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
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

	storedAddrs, _ := s.platformCfg.Get(r.Context(), platformconfig.KeyShellyAddresses)
	storedPassword, _ := s.platformCfg.GetSecret(r.Context(), platformconfig.KeyShellyPassword)
	s.SetShellyClients(BuildShellyClients(storedAddrs, storedPassword))

	data := s.buildSettingsData(r)
	data.Flash = "Saved."
	data.FlashType = "green"
	s.renderAdminPage(w, "settings", data)
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
