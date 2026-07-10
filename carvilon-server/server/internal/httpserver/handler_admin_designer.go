package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"carvilon.local/server/internal/gpio"
	"carvilon.local/server/internal/hostinfo"
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
	_ = json.NewEncoder(w).Encode(map[string]any{
		"blocks": designer.Catalog(gpio.Enabled(), sysMetricsForCatalog(), s.nfcReadersForCatalog(r.Context()), s.mqttBrokerRunning(), s.telegramRunning(), s.shellyDevicesForCatalog(r.Context())),
	})
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
		caps := shellycaps.Channels(d.Model)
		chans := make([]designer.ShellyChannel, 0, len(caps))
		for _, c := range caps {
			chans = append(chans, designer.ShellyChannel{ID: c.ID, Meter: c.Meter})
		}
		out = append(out, designer.ShellyDevice{
			ID:       d.ID,
			MAC:      d.MAC,
			Name:     shellyDisplayName(d),
			Model:    d.Model,
			Prefix:   mqttstore.DefaultPrefix(username),
			Channels: chans,
		})
	}
	return out
}

// shellyDisplayName picks the module's label: the device's name, else its
// model, else a MAC-based fallback - the same precedence the catalog's
// shellyBlocks uses, resolved here so the block carries a ready label.
func shellyDisplayName(d shellystore.Device) string {
	switch {
	case strings.TrimSpace(d.Name) != "":
		return d.Name
	case strings.TrimSpace(d.Model) != "":
		return d.Model
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
