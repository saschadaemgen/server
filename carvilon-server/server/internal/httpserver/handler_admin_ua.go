// Saison 21 - UA-Integration Etappe 1: a strictly READ-ONLY overview
// of the UniFi Access installation (hubs, readers, viewers, doors),
// grouped by hub, fetched live from the UA developer API.
//
// CARVILON is the master database; UA is only hardware we read here -
// nothing on this page controls a door, changes a setting, or adopts
// a device into any carvilon registry. Later etappes add control,
// reader adoption and events; this one just shows what is there.
//
// Gating: the page only talks to the UDM when the "UA aktiv" toggle is
// on AND a client is configured (base URL + token). Everything else is
// a clean hint. The token/host never reach the log or the HTML - only
// fixed friendly strings do.
package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"carvilon.local/server/internal/uaapi"
)

// uaOverviewData is the payload for templates/admin/ua.html.
type uaOverviewData struct {
	User adminUser

	Enabled    bool // "UA aktiv"-Schalter an
	Configured bool // + client wired (base URL + token stored)

	// Terminal error states (devices could not be listed at all).
	Unauthorized bool   // 401 from the UDM -> token hint
	LoadError    string // any other devices-fetch failure -> unreachable hint

	// Section-level, non-fatal: devices loaded but doors did not.
	DoorsError string

	Emergency *uaEmergencyView

	Hubs         []uaHubGroup
	Ungrouped    uaHubGroup
	HasUngrouped bool

	HubCount    int
	ReaderCount int
	ViewerCount int
	DoorCount   int
	DeviceCount int
}

// uaHubGroup is one hub with everything attached to it. For the
// Ungrouped pseudo-group Hub is nil.
type uaHubGroup struct {
	Hub     *uaDeviceRow
	Readers []uaDeviceRow
	Viewers []uaDeviceRow
	Others  []uaDeviceRow
	Doors   []uaDoorRow
}

// uaDeviceRow is the display shape of one UA device. The collapsed row
// shows Name/Type/Nature/Online; the expanded panel shows the IDs,
// state flags, capabilities (all already in the page) plus the lazily
// fetched /settings.
type uaDeviceRow struct {
	ID             string // bare id / MAC (path for the settings fetch)
	MAC            string // colon form, for display
	Name           string
	Type           string
	Nature         string // "hub" | "reader" | "viewer" | "other"
	NatureLabel    string
	Online         bool
	Adopted        bool
	Managed        bool
	Connected      bool
	LocationID     string
	ConnectedUAHID string
	Capabilities   []string

	// Viewer-only: is this UA-reported viewer one of CARVILON's own
	// mock viewers (its MAC is in our viewers table) or a foreign UA
	// viewer?
	ViewerKind      string // "" | "mock" | "ua"
	ViewerKindLabel string
}

// uaDoorRow is the display shape of one door under a hub.
type uaDoorRow struct {
	ID            string
	Name          string
	Type          string
	HubID         string
	Floor         string
	Position      string // "geöffnet" | "geschlossen" | raw | "unbekannt"
	PositionState string // "open" | "closed" | "unknown"
	Lock          string // "verriegelt" | "entriegelt" | raw | "unbekannt"
	LockState     string // "locked" | "unlocked" | "unknown"
	BoundToHub    string // "ja" | "nein" | "unbekannt"
}

// uaEmergencyView is the global emergency banner state.
type uaEmergencyView struct {
	Active       bool
	ActiveLabels []string
	Rows         []kvRow
}

// kvRow is one key/value line in an expanded detail panel. Rendered by
// html/template as escaped text, so raw UA field values are safe.
type kvRow struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// uaSection is one titled block of key/value detail returned by the
// lazy detail endpoints (device settings, door detail, lock rule).
type uaSection struct {
	Title string  `json:"title"`
	Rows  []kvRow `json:"rows"`
	Error string  `json:"error,omitempty"`
}

// handleAdminUA renders the read-only UA device + door overview.
func (s *Server) handleAdminUA(w http.ResponseWriter, r *http.Request) {
	username := AdminUserFromContext(r.Context())
	data := uaOverviewData{
		User:    adminUser{Name: username, Initials: initialsOf(username)},
		Enabled: s.uaEnabled(r.Context()),
	}
	// UA off -> no calls at all, just the hint (Etappe-1 contract).
	if !data.Enabled {
		s.renderAdminPage(w, "ua", data)
		return
	}
	data.Configured = s.ua != nil
	if !data.Configured {
		s.renderAdminPage(w, "ua", data)
		return
	}
	s.buildUAOverview(r.Context(), &data)
	s.renderAdminPage(w, "ua", data)
}

// buildUAOverview fetches devices (with ?refresh=true), doors and the
// emergency status and groups them. Each fetch is isolated: a doors
// failure does not blank the (already loaded) devices, and an
// emergency failure just drops the banner.
func (s *Server) buildUAOverview(ctx context.Context, data *uaOverviewData) {
	devices, err := s.ua.ListDevicesRefresh(ctx)
	if err != nil {
		if errors.Is(err, uaapi.ErrUnauthorized) {
			data.Unauthorized = true
		} else {
			s.log.Warn("ua overview: list devices failed", "err", err)
			data.LoadError = uaFriendlyError(err)
		}
		return
	}

	doors, derr := s.ua.ListDoors(ctx)
	if derr != nil {
		s.log.Warn("ua overview: list doors failed", "err", derr)
		data.DoorsError = uaFriendlyError(derr)
		doors = nil
	}

	if em, eerr := s.ua.EmergencySettings(ctx); eerr != nil {
		s.log.Warn("ua overview: emergency status failed", "err", eerr)
	} else if em != nil {
		data.Emergency = buildEmergencyView(em)
	}

	s.groupUA(data, devices, doors, s.viewerMACSet(ctx))
}

// groupUA sorts devices and doors under their hubs. Hubs are created
// first (as group anchors) so the slice never grows while element
// pointers are held.
func (s *Server) groupUA(data *uaOverviewData, devices []uaapi.Device, doors []uaapi.Door, mockMACs map[string]bool) {
	data.DeviceCount = len(devices)
	data.DoorCount = len(doors)

	hubIndex := make(map[string]int) // hub device id -> data.Hubs index
	for _, d := range devices {
		if d.Nature() != "hub" {
			continue
		}
		row := makeUADeviceRow(d, mockMACs)
		data.Hubs = append(data.Hubs, uaHubGroup{Hub: &row})
		hubIndex[d.ID] = len(data.Hubs) - 1
		data.HubCount++
	}

	ungrouped := &uaHubGroup{}
	groupFor := func(hubID string) *uaHubGroup {
		if hubID != "" {
			if idx, ok := hubIndex[hubID]; ok {
				return &data.Hubs[idx]
			}
		}
		return ungrouped
	}

	for _, d := range devices {
		if d.Nature() == "hub" {
			continue
		}
		row := makeUADeviceRow(d, mockMACs)
		grp := groupFor(d.ConnectedUAHID)
		switch row.Nature {
		case "reader":
			grp.Readers = append(grp.Readers, row)
			data.ReaderCount++
		case "viewer":
			grp.Viewers = append(grp.Viewers, row)
			data.ViewerCount++
		default:
			grp.Others = append(grp.Others, row)
		}
	}

	for _, dr := range doors {
		hubID := dr.HubID
		if hubID == "" {
			// some firmwares carry the binding hub id in is_bind_hub.
			if cand := dr.BindHubRaw(); cand != "" {
				if _, ok := hubIndex[cand]; ok {
					hubID = cand
				}
			}
		}
		grp := groupFor(hubID)
		grp.Doors = append(grp.Doors, makeUADoorRow(dr))
	}

	if len(ungrouped.Readers)+len(ungrouped.Viewers)+len(ungrouped.Others)+len(ungrouped.Doors) > 0 {
		data.Ungrouped = *ungrouped
		data.HasUngrouped = true
	}
}

// makeUADeviceRow builds the display row for a device, tagging viewers
// as CARVILON mock vs foreign UA by MAC.
func makeUADeviceRow(d uaapi.Device, mockMACs map[string]bool) uaDeviceRow {
	row := uaDeviceRow{
		ID:             d.ID,
		MAC:            d.DisplayMAC(),
		Name:           d.DisplayName(),
		Type:           d.Type,
		Nature:         d.Nature(),
		NatureLabel:    uaNatureLabel(d.Nature()),
		Online:         d.IsOnline,
		Adopted:        d.IsAdopted,
		Managed:        d.IsManaged,
		Connected:      d.IsConnected,
		LocationID:     d.LocationID,
		ConnectedUAHID: d.ConnectedUAHID,
		Capabilities:   d.Capabilities,
	}
	if row.Nature == "viewer" {
		if mockMACs[normalizeMACToColonForm(d.ID)] {
			row.ViewerKind, row.ViewerKindLabel = "mock", "CARVILON Mock-Viewer"
		} else {
			row.ViewerKind, row.ViewerKindLabel = "ua", "UA-Viewer"
		}
	}
	return row
}

// makeUADoorRow builds the display row for a door.
func makeUADoorRow(d uaapi.Door) uaDoorRow {
	row := uaDoorRow{
		ID:            d.ID,
		Name:          d.DisplayName(),
		Type:          d.Type,
		HubID:         d.HubID,
		Floor:         d.FloorLabel(),
		PositionState: d.PositionState(),
		LockState:     d.LockState(),
		Position:      uaPositionLabel(d.PositionState(), d.PositionRaw()),
		Lock:          uaLockLabel(d.LockState(), d.LockRaw()),
		BoundToHub:    uaBoundLabel(d),
	}
	return row
}

func uaNatureLabel(nature string) string {
	switch nature {
	case "hub":
		return "Hub / Tür-Controller"
	case "reader":
		return "Reader"
	case "viewer":
		return "Viewer"
	default:
		return "Gerät"
	}
}

func uaPositionLabel(state, raw string) string {
	switch state {
	case "open":
		return "geöffnet"
	case "closed":
		return "geschlossen"
	default:
		if raw != "" {
			return raw
		}
		return "unbekannt"
	}
}

func uaLockLabel(state, raw string) string {
	switch state {
	case "locked":
		return "verriegelt"
	case "unlocked":
		return "entriegelt"
	default:
		if raw != "" {
			return raw
		}
		return "unbekannt"
	}
}

func uaBoundLabel(d uaapi.Door) string {
	if v, ok := d.BoundToHub(); ok {
		if v {
			return "ja"
		}
		return "nein"
	}
	return "unbekannt"
}

// viewerMACSet reads every MAC in our viewers table (the "mock" table)
// as a set of canonical colon-form MACs, for the UA-vs-mock viewer
// tagging. A read failure degrades to an empty set (all UA viewers
// then read as foreign) rather than failing the page.
func (s *Server) viewerMACSet(ctx context.Context) map[string]bool {
	set := make(map[string]bool)
	if s.platformCfg == nil {
		return set
	}
	rows, err := s.platformCfg.DB().QueryContext(ctx, `SELECT mac FROM viewers`)
	if err != nil {
		s.log.Warn("ua overview: viewer mac lookup failed", "err", err)
		return set
	}
	defer rows.Close()
	for rows.Next() {
		var mac string
		if err := rows.Scan(&mac); err != nil {
			continue
		}
		if n := normalizeMACToColonForm(mac); n != "" {
			set[n] = true
		}
	}
	return set
}

// buildEmergencyView turns the /doors/settings/emergency payload into a
// banner state. "Active" is decided conservatively from boolean / known
// string flags only (never numbers, so a timestamp field can't read as
// an emergency); the full detail is always shown regardless.
func buildEmergencyView(v any) *uaEmergencyView {
	ev := &uaEmergencyView{Rows: flattenUADetail(v)}
	if m, ok := v.(map[string]any); ok {
		for k, val := range m {
			if uaEmergencyFlag(val) {
				ev.Active = true
				ev.ActiveLabels = append(ev.ActiveLabels, k)
			}
		}
		sort.Strings(ev.ActiveLabels)
	}
	return ev
}

func uaEmergencyFlag(val any) bool {
	switch t := val.(type) {
	case bool:
		return t
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "on", "active", "enabled", "lockdown", "evacuation", "true", "yes":
			return true
		}
	}
	return false
}

// handleAdminUADeviceSettings lazily serves the /devices/:id/settings
// detail for one device as JSON, loaded when its row is expanded.
func (s *Server) handleAdminUADeviceSettings(w http.ResponseWriter, r *http.Request) {
	id, ok := s.uaDetailPrelude(w, r)
	if !ok {
		return
	}
	sec := uaSection{Title: "Zutrittsmethoden"}
	if v, err := s.ua.DeviceSettings(r.Context(), id); err != nil {
		s.log.Warn("ua overview: device settings failed", "err", err)
		sec.Error = uaFriendlyError(err)
	} else {
		sec.Rows = flattenUADetail(v)
	}
	writeUADetail(w, sec)
}

// handleAdminUADoorDetail lazily serves the full door record plus its
// lock rule as JSON, loaded when a door row is expanded.
func (s *Server) handleAdminUADoorDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := s.uaDetailPrelude(w, r)
	if !ok {
		return
	}
	detail := uaSection{Title: "Tür-Details"}
	if v, err := s.ua.DoorDetail(r.Context(), id); err != nil {
		s.log.Warn("ua overview: door detail failed", "err", err)
		detail.Error = uaFriendlyError(err)
	} else {
		detail.Rows = flattenUADetail(v)
	}

	rule := uaSection{Title: "Sperrregel"}
	if v, err := s.ua.DoorLockRule(r.Context(), id); err != nil {
		// A door with no rule set answers not-found; that is a clean
		// "keine Regel", not an error.
		if !errors.Is(err, uaapi.ErrNotFound) {
			s.log.Warn("ua overview: door lock rule failed", "err", err)
			rule.Error = uaFriendlyError(err)
		}
	} else {
		rule.Rows = flattenUADetail(v)
	}
	writeUADetail(w, detail, rule)
}

// uaDetailPrelude gates a lazy detail request: sets JSON headers,
// enforces the UA-ready precondition and validates the id. Returns the
// id and ok=false when it already wrote the response.
func (s *Server) uaDetailPrelude(w http.ResponseWriter, r *http.Request) (string, bool) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if !s.uaReady(r.Context()) {
		writeUADetailError(w, "UA ist nicht aktiv oder nicht konfiguriert.")
		return "", false
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if !uaValidID(id) {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return "", false
	}
	return id, true
}

func (s *Server) uaReady(ctx context.Context) bool {
	return s.uaEnabled(ctx) && s.ua != nil
}

// uaValidID accepts only the id shapes the UA API uses (a bare MAC or a
// UUID-ish token): letters, digits and : . _ -. Defence in depth on top
// of the url.PathEscape the client already applies.
//
// A ".." run is rejected outright: url.PathEscape leaves dots untouched,
// and the Go HTTP client does not collapse dot-segments in an outgoing
// request path, so a ".." id would send a literal "/../" to the UDM.
// (Go's ServeMux cleans a literal "/../" in the incoming path but NOT a
// percent-encoded "%2e%2e", which decodes to ".." in PathValue.) No real
// UA id contains "..", so this only blocks traversal probing.
func uaValidID(id string) bool {
	if id == "" || len(id) > 128 || strings.Contains(id, "..") {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == ':' || c == '.' || c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}

func writeUADetail(w http.ResponseWriter, sections ...uaSection) {
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "sections": sections})
}

func writeUADetailError(w http.ResponseWriter, msg string) {
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg})
}

// uaFriendlyError maps a uaapi error to a fixed German message. It
// never embeds the underlying error text, which can carry the UDM host
// - the token/host must never reach the HTML or JSON.
func uaFriendlyError(err error) string {
	switch {
	case errors.Is(err, uaapi.ErrUnauthorized):
		return "Zugriff verweigert – bitte den UA-API-Token prüfen (401)."
	case errors.Is(err, uaapi.ErrNotFound):
		return "Nicht gefunden."
	default:
		return "UDM nicht erreichbar oder Antwort ungültig."
	}
}

// flattenUADetail turns an arbitrary decoded UA payload into a sorted
// list of key/value lines, so the detail views show everything the API
// returned without a typed schema. Nested objects/arrays are dotted /
// indexed; empty containers keep a placeholder so a present-but-empty
// field is not silently dropped.
func flattenUADetail(v any) []kvRow {
	var rows []kvRow
	flattenUAInto("", v, &rows)
	sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
	return rows
}

func flattenUAInto(prefix string, v any, out *[]kvRow) {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			if prefix != "" {
				*out = append(*out, kvRow{Key: prefix, Value: "{}"})
			}
			return
		}
		for k, val := range t {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			flattenUAInto(key, val, out)
		}
	case []any:
		if len(t) == 0 {
			if prefix != "" {
				*out = append(*out, kvRow{Key: prefix, Value: "[]"})
			}
			return
		}
		for i, val := range t {
			flattenUAInto(prefix+"["+strconv.Itoa(i)+"]", val, out)
		}
	case nil:
		*out = append(*out, kvRow{Key: prefix, Value: "—"})
	case bool:
		*out = append(*out, kvRow{Key: prefix, Value: uaBoolLabel(t)})
	case float64:
		*out = append(*out, kvRow{Key: prefix, Value: strconv.FormatFloat(t, 'f', -1, 64)})
	case json.Number:
		*out = append(*out, kvRow{Key: prefix, Value: t.String()})
	case string:
		*out = append(*out, kvRow{Key: prefix, Value: t})
	default:
		*out = append(*out, kvRow{Key: prefix, Value: ""})
	}
}

func uaBoolLabel(b bool) string {
	if b {
		return "ja"
	}
	return "nein"
}
