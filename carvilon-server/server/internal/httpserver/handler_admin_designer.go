package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"carvilon.local/server/internal/gpio"
	"carvilon.local/server/internal/hostinfo"
	"carvilon.local/server/internal/mideaengine"
	"carvilon.local/server/internal/mideastore"
	"carvilon.local/server/internal/mqttstore"
	"carvilon.local/server/internal/nfc"
	"carvilon.local/server/internal/shellycaps"
	"carvilon.local/server/internal/shellystore"
	"carvilon.local/server/internal/sysmetrics"
	"carvilon.local/server/web/designer"
)

// designerData is the payload for the /a/designer host page. The editor
// itself lives entirely inside the iframe (its own document, CSS and
// JS); the host page only needs the admin user for the shared topbar
// plus the optional ?g=<id> graph deep link forwarded into the iframe.
type designerData struct {
	User    adminUser
	GraphID string
}

// handleAdminDesigner renders the logic-editor host page: the shared
// Saison-20 admin layout (topbar) wrapping a full-bleed iframe that
// loads the embedded editor from /a/designer/. The iframe gives the
// editor a clean isolation boundary from the admin shell's tokens and
// scripts. A ?g=<id> deep link (the hook the later Tags page pulls on)
// passes through onto the iframe src; only a plain integer is
// forwarded.
func (s *Server) handleAdminDesigner(w http.ResponseWriter, r *http.Request) {
	username := AdminUserFromContext(r.Context())
	g := r.URL.Query().Get("g")
	if _, err := strconv.ParseInt(g, 10, 64); err != nil {
		g = ""
	}
	s.renderAdminPage(w, "designer", designerData{
		User:    adminUser{Name: username, Initials: initialsOf(username)},
		GraphID: g,
	})
}

// handleDesignerCatalog serves the designer building-block catalog as
// JSON for the editor palette. Route: GET /a/designer/catalog.json
// (requireAdminSession) — the more specific pattern wins over the
// /a/designer/ static subtree. The catalog is the single source of
// truth for the 111 palette blocks; the four implemented ones derive
// their ports/params from the engine registry.
func (s *Server) handleDesignerCatalog(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	// GPIO, the system category, NFC, MQTT and Telegram all follow
	// runtime detection: each appears in the palette only when the
	// host/broker/bot exposes it. NFC needs a reader detected on an I2C
	// bus at startup; MQTT needs the broker actually running (the mqtt:
	// driver binds to its in-process inline client); Telegram needs the
	// bot enabled with a token set (the telegram: driver binds to the
	// manager's Conn).
	blocks := designer.Catalog(gpio.Enabled(), sysMetricsForCatalog(), s.nfcReadersForCatalog(r.Context()), s.mqttBrokerRunning(), s.telegramRunning(), s.shellyDevicesForCatalog(r.Context()), s.readoutDevicesForCatalog(r.Context())...)
	blocks = append(blocks, s.mideaControlLoopBlocks(r.Context())...)
	_ = json.NewEncoder(w).Encode(map[string]any{"blocks": blocks})
}

// mideaControlLoopHelp is the block's plain-language help, shown on the block
// (help icon) and in the node inspector. It is deliberately the ONE source for
// both surfaces, so the editor cannot drift from what the loop really does.
const mideaControlLoopHelp = "Climate control loop. Compares the room temperature from your own external " +
	"sensor with the target temperature and drives this device's setpoint, mode and fan whenever the " +
	"decision actually changes. It reads the device's built-in return-air sensor by itself — that needs " +
	"no wire.\n\n" +
	"Minimal setup: wire an external room sensor into \"Room temperature\", dial the target in with the " +
	"up/down rocker on the block (17–30 °C, half-degree steps), and switch Enable to ON. No constant blocks " +
	"needed. Enable off does not merely stop the loop — it switches the device off.\n\n" +
	"Wire \"Target temperature\" or \"Enable\" only if the graph should own them: a wired port always wins " +
	"and the matching control on the block goes inert. The ports on the right are read-only readouts.\n\n" +
	"Advanced profile only — switching the device back to Standard stops the drive (the readouts keep " +
	"computing). While this loop runs it is the single driver: the device's manual controls are locked."

// mideaControlLoopBlocks emits one "control loop" editor block per adopted Midea
// device that is in the ADVANCED profile (E2). The block is the registered
// midea.control_loop engine node (ports from its descriptor); the device id is
// baked into the Channel field, which the editor maps to the node's "device"
// param. Switching a device to standard removes its block on the next catalog
// load - the E1 profile toggle gates the advanced control loop.
func (s *Server) mideaControlLoopBlocks(ctx context.Context) []designer.CatalogBlock {
	if s.mideastore == nil {
		return nil
	}
	act, err := s.mideastore.ListActive(ctx)
	if err != nil {
		return nil
	}
	var out []designer.CatalogBlock
	for _, d := range act {
		if d.Profile != mideastore.ProfileAdvanced {
			continue
		}
		out = append(out, designer.CatalogBlock{
			Type: mideaengine.TypeControlLoop,
			// Its own category + icon: the control loop must not be mistaken
			// for the device block (which is the raw remote — category
			// "climate", snowflake). This one is the controller.
			Category:    "climate-loop",
			Title:       "Control loop · " + mideaDisplayName(d),
			Icon:        "gauge",
			Channel:     d.ID, // baked device id → the node's "device" param
			Implemented: true,
			Description: mideaControlLoopHelp,
		})
	}
	return out
}

// readoutDevicesForCatalog bridges every adopted readout/sensor device to
// the designer catalog's generic ReadoutDevice type, so any such device
// gets a capability-driven editor block (output ports + live faceplate)
// with no per-vendor catalog code - the generalisation of
// shellyDevicesForCatalog beyond one vendor. UniFi Protect UP-Sense sensors
// are the first source (via the persistent poller's snapshot); a future
// readout module or the climate controller appends here and gets its blocks
// for free. Each readout already carries its fully-formed physical channel
// ref (e.g. "protect:<id>:temperature"), so the editor bakes it straight
// into the expanded source node and the run binds it by prefix.
func (s *Server) readoutDevicesForCatalog(ctx context.Context) []designer.ReadoutDevice {
	var out []designer.ReadoutDevice
	if s.protectMonitor != nil {
		for _, d := range s.protectMonitor.Devices() {
			rd := designer.ReadoutDevice{ID: d.ID, Class: "sensor", Name: d.Name, Model: d.Model, Icon: "thermometer"}
			for _, ro := range d.Readouts {
				rd.Readouts = append(rd.Readouts, designer.ReadoutPort{
					Key:     ro.Token,
					Label:   ro.Label,
					Unit:    ro.Unit,
					Kind:    ro.KindString(),
					Channel: ro.Channel,
				})
			}
			out = append(out, rd)
		}
	}
	// Midea climate controllers: a capability-driven DEVICE module - readout
	// OUTPUT ports (sensor) PLUS control INPUT ports (setpoint/mode/fan). This
	// is the readout path generalised to control capabilities and a non-Shelly
	// module; the same live monitor backs the editor and the cockpit.
	if s.mideaMon != nil {
		names := map[string]string{}
		if s.mideastore != nil {
			if act, err := s.mideastore.ListActive(ctx); err == nil {
				for _, d := range act {
					names[d.ID] = mideaDisplayName(d)
				}
			}
		}
		for _, d := range s.mideaMon.Devices() {
			name := names[d.ID]
			if name == "" {
				name = "Midea " + d.ID
			}
			rd := designer.ReadoutDevice{ID: d.ID, Class: "climate", Name: name, Model: d.Model, Icon: "snowflake"}
			for _, ro := range d.Readouts {
				rd.Readouts = append(rd.Readouts, designer.ReadoutPort{
					Key: ro.Token, Label: ro.Label, Unit: ro.Unit, Kind: ro.Kind, Channel: ro.Channel,
				})
			}
			for _, c := range d.Controls {
				rd.Controls = append(rd.Controls, designer.ControlPort{
					Key: c.Token, Label: c.Label, Unit: c.Unit, Kind: c.Kind, Options: c.Options, Channel: c.Channel,
				})
			}
			out = append(out, rd)
		}
	}
	return out
}

// shellyDevicesForCatalog bridges the adopted Shelly device set to the
// designer catalog's neutral type: one finished module per active device,
// with its MQTT topic prefix and its capability-derived channel set (the
// editor builds the module's ports + faceplate + mqtt: bindings from
// this). The topic prefix uses the device's provisioned broker username
// when known, else the conventional "shelly-<mac>" the provisioner
// assigns - so the module can compose "carvilon/shelly-<mac>/..." even
// before the MQTT link row is populated. Channels are model-derived for
// M1 (offline, deterministic); a live switch:N enumeration refines this
// later. The Shelly category needs the broker running (the module's
// bindings ride the mqtt: driver), so it is empty when the broker is off.
func (s *Server) shellyDevicesForCatalog(ctx context.Context) []designer.ShellyDevice {
	if s.shellystore == nil || !s.mqttBrokerRunning() {
		return nil
	}
	devs, err := s.shellystore.ListActive(ctx)
	if err != nil {
		s.log.Error("shelly devices for catalog", "err", err)
		return nil
	}
	out := make([]designer.ShellyDevice, 0, len(devs))
	for _, d := range devs {
		username := d.MQTTUsername
		if username == "" && d.MAC != "" {
			username = "shelly-" + strings.ToLower(d.MAC)
		}
		if username == "" {
			continue // no identity to build a topic prefix from
		}
		// Generation decides the capability table AND the topic root: Gen1
		// firmware pins its topics under shellies/<mqtt_id> (provisioning
		// sets mqtt_id = the broker username), Gen2+ under the assigned
		// carvilon/<user> prefix. An unclassified device modules as Gen2
		// (the pre-Gen1 behaviour) until its identify probe says otherwise.
		prefix := mqttstore.DefaultPrefix(username)
		gen := 0
		var chans []designer.ShellyChannel
		if d.Gen == shellystore.Gen1 {
			prefix = "shellies/" + username
			gen = shellystore.Gen1
			// A Gen1 device is either relay-class (switches) or light-class
			// (RGBW2). Light channels carry their mode as Kind so the editor
			// builds the light module (on/off + gain, color/status readouts)
			// against the color-mode topic set. Mode "" renders the device's
			// default shape (color for the RGBW2).
			for _, c := range shellycaps.Gen1Channels(d.Model, "") {
				chans = append(chans, designer.ShellyChannel{ID: c.ID, Meter: c.Meter})
			}
			for _, l := range shellycaps.Gen1Lights(d.Model, "") {
				chans = append(chans, designer.ShellyChannel{ID: l.ID, Kind: l.Kind})
			}
			if len(chans) == 0 {
				continue // an unknown Gen1 model with no derivable channels
			}
		} else {
			for _, c := range shellycaps.Channels(d.Model) {
				chans = append(chans, designer.ShellyChannel{ID: c.ID, Meter: c.Meter})
			}
		}
		out = append(out, designer.ShellyDevice{
			ID:       d.ID,
			MAC:      d.MAC,
			HistID:   shellystore.HistoryID(d.MAC),
			Name:     shellyDisplayName(d),
			Model:    shellyCatalogModel(d),
			Gen:      gen,
			Prefix:   prefix,
			Channels: chans,
		})
	}
	return out
}

// shellyCatalogModel renders the model for the module label: Gen1 rows
// store the raw type code ("SHSW-25"), which reads as its human name.
func shellyCatalogModel(d shellystore.Device) string {
	if d.Gen == shellystore.Gen1 {
		return shellycaps.Gen1ModelLabel(d.Model)
	}
	return d.Model
}

// shellyDisplayName picks the module's label: the device's name, else its
// model, else a MAC-based fallback - the same precedence the catalog's
// shellyBlocks uses, resolved here so the block carries a ready label.
func shellyDisplayName(d shellystore.Device) string {
	switch {
	case strings.TrimSpace(d.Name) != "":
		return d.Name
	case strings.TrimSpace(d.Model) != "":
		return shellyCatalogModel(d)
	default:
		return "Shelly " + d.MAC
	}
}

// mqttBrokerRunning reports whether the embedded broker is wired and up,
// gating the editor's MQTT palette category (the mqtt: driver can only
// bind when the broker's inline client is available).
func (s *Server) mqttBrokerRunning() bool {
	return s.mqtt != nil && s.mqtt.Status().Running
}

// telegramRunning reports whether the bot is wired and its poll loop is
// up (enabled + token set - a boot-time fact, not cloud reachability),
// gating the editor's Telegram palette category.
func (s *Server) telegramRunning() bool {
	return s.telegram != nil && s.telegram.Status().Running
}

// sysMetricsForCatalog bridges the sys: driver's available metrics to the
// designer catalog's neutral type, so the catalog package stays unaware of
// the driver.
func sysMetricsForCatalog() []designer.SysMetric {
	ms := sysmetrics.Metrics()
	out := make([]designer.SysMetric, 0, len(ms))
	for _, m := range ms {
		out = append(out, designer.SysMetric{Address: m.Address, Label: m.Label, Unit: m.Unit})
	}
	return out
}

// nfcReadersForCatalog bridges the nfc: driver's detected readers to the
// designer catalog's neutral type, joining in each reader's display name
// from the registry (the operator's custom name overrides the speaking
// auto-name) so the palette blocks read the same as the Device Center. The
// catalog package stays unaware of the driver and the registry.
func (s *Server) nfcReadersForCatalog(ctx context.Context) []designer.NFCReader {
	rs := nfc.Readers()
	names := map[string]string{}
	if s.readerStore != nil {
		if list, err := s.readerStore.List(ctx); err == nil {
			for _, rd := range list {
				names[rd.ID] = rd.DisplayName()
			}
		}
	}
	out := make([]designer.NFCReader, 0, len(rs))
	for _, r := range rs {
		out = append(out, designer.NFCReader{
			ID:             r.ID,
			Name:           names[r.Identity],
			UIDChannel:     r.UIDChannel,
			PresentChannel: r.PresentChannel,
		})
	}
	return out
}

// handleDesignerGPIOLines serves the detected GPIO lines (offset, name,
// in-use) for the editor's pin picker. Route: GET /a/designer/gpio/lines
// (requireAdminSession). Empty on a non-GPIO host.
func (s *Server) handleDesignerGPIOLines(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	lines := gpio.Lines()
	if lines == nil {
		lines = []gpio.LineInfo{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"lines": lines})
}

// handleDesignerTelegramChats serves the allowlisted Telegram chats
// for the editor's chat picker. Route: GET /a/designer/telegram/chats
// (requireAdminSession). Chat ids travel as strings - int64 chat ids
// can exceed JavaScript's safe-integer range. Empty when the bot is
// not wired.
func (s *Server) handleDesignerTelegramChats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	type chatItem struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}
	chats := []chatItem{}
	if s.telegramStore != nil {
		list, err := s.telegramStore.ListAllowed(r.Context())
		if err != nil {
			s.log.Error("telegram chats for picker", "err", err)
		}
		for _, c := range list {
			chats = append(chats, chatItem{ID: strconv.FormatInt(c.ChatID, 10), Label: c.Label})
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"chats": chats})
}

// handleDesignerHost serves a human description of the host the server
// runs on (Pi model / distro / kernel / arch) for the editor's status bar,
// replacing the former "Miniserver online" placeholder. Route:
// GET /a/designer/host (requireAdminSession).
func (s *Server) handleDesignerHost(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_ = json.NewEncoder(w).Encode(hostinfo.Detect())
}

// designerStaticHandler serves the embedded editor bundle under
// /a/designer/. index.html is the directory index; the ES modules under
// js/, the css/, and the vendored Lucide/font assets are served verbatim
// from the same FS. A request to /a/designer/ resolves to index.html.
//
// Content-Type is set explicitly per extension because Go's mime table
// is OS-dependent (the Windows registry can map .js to text/plain, which
// browsers reject for module scripts under strict MIME checking). woff2
// is set for the same reason the /static/ asset gate does. The bundle is
// small and changes only on deploy, so a plain no-cache keeps it simple
// without a cache-busting token.
func designerStaticHandler() http.Handler {
	file := http.FileServer(http.FS(designer.FS))
	served := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".woff2"):
			w.Header().Set("Content-Type", "font/woff2")
		case strings.HasSuffix(r.URL.Path, ".js"):
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		case strings.HasSuffix(r.URL.Path, ".css"):
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		}
		w.Header().Set("Cache-Control", "no-cache")
		file.ServeHTTP(w, r)
	})
	return http.StripPrefix("/a/designer/", served)
}
