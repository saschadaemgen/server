// Saison 21 - Device Center (UA-Integration Etappe 1, presentation
// redesign): a strictly READ-ONLY overview of the UniFi Access
// installation (hubs, readers, viewers, doors), rendered as one flat,
// filterable device table with a left filter column and a right
// slide-out detail panel.
//
// CARVILON is the master database; UA is only hardware we read here -
// nothing on this page controls a door, changes a setting, or adopts
// a device into any carvilon registry. Later etappes add control,
// reader adoption and events; this one just shows what is there.
//
// The design ships a "Cameras" and "Sensors" category too, but there is
// no backend for them yet (UniFi Protect / sensors are a later ticket),
// so those categories render empty/disabled - no invented data. Only
// the "UniFi" source is real; the Source facet is built so further
// sources are pure additions later.
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

// uaOverviewData is the payload for templates/admin/ua.html (the Device
// Center). Rows are a flat, pre-sorted device+door list; facets carry
// the server-computed initial counts for the left filter column. All
// live filtering/sorting is client-side over the rendered rows.
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

	Rows []uaRow

	CategoryFacets []uaFacet
	SourceFacets   []uaFacet
	StatusFacets   []uaFacet
	ModelFacets    []uaFacet

	// Fleet-status counters (two-digit displays in the left column).
	OnlineCount  int
	OfflineCount int
	UpdatesCount int
	TotalCount   int

	// Two-char, clamped forms for the flip-digit displays (00..99).
	OnlinePad  string
	OfflinePad string
	UpdatesPad string
}

// uaFacet is one filter row in the left column: a value, its display
// label and its current count. Disabled facets (Cameras/Sensors) are
// part of the shell but carry no data yet.
type uaFacet struct {
	Key      string
	Label    string
	Count    int
	Disabled bool
}

// uaRow is one row in the flat device table. It carries both the
// display fields and the lowercased search haystack + filter keys the
// client uses. Kind selects the lazy detail endpoint and the panel
// behaviour ("device" vs "door").
type uaRow struct {
	ID   string // bare id / MAC (path for the lazy detail fetch)
	Kind string // "device" | "door"

	Category  string // "hub" | "reader" | "viewer" | "other" | "door"
	TypeLabel string // singular type label: "Hub" | "Reader" | ...

	Index int // 1-based position in the (pre-sorted) flat list

	// Group-header markers: the first row of each category carries the
	// group label and the category's total count so the template can
	// emit a group heading before it.
	GroupStart bool
	GroupLabel string
	GroupCount int

	Name string
	Mock bool // viewer that is one of CARVILON's own mock viewers

	// Status. Devices use "online"/"offline"; doors use the lock state
	// ("locked"/"unlocked"/"unknown"). StatusText is the display string.
	StatusState string
	StatusText  string

	// Door-only secondary status (door-position sensor).
	Position      string
	PositionState string // "open" | "closed" | "unknown"

	Source      string // "unifi"
	SourceLabel string // "UniFi"

	Model    string // device model / type (also the Model facet key)
	IP       string
	Firmware string
	Version  string
	MAC      string
	Uptime   string

	// Panel: the known fields shown immediately (the lazy /settings,
	// door-detail and lock-rule sections load when the panel opens).
	Detail       []kvRow
	Capabilities []string

	// Lowercased "name model ip mac" for the client search box.
	Search string
}

// uaEmergencyView is the global emergency banner state.
type uaEmergencyView struct {
	Active       bool
	ActiveLabels []string
	Rows         []kvRow
}

// kvRow is one key/value line in a detail panel. Rendered by
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

// categoryOrder ranks the natures for the flat table (hubs first, doors
// last) so the pre-sorted rows read top-down like the topology.
var categoryOrder = map[string]int{"hub": 0, "reader": 1, "viewer": 2, "other": 3, "door": 4}

// handleAdminUA renders the read-only Device Center.
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
// emergency status and flattens them into rows + facets. Each fetch is
// isolated: a doors failure does not blank the (already loaded)
// devices, and an emergency failure just drops the banner.
func (s *Server) buildUAOverview(ctx context.Context, data *uaOverviewData) {
	devices, err := s.ua.ListDevicesRefresh(ctx)
	if err != nil {
		if errors.Is(err, uaapi.ErrUnauthorized) {
			data.Unauthorized = true
		} else {
			s.log.Warn("device center: list devices failed", "err", err)
			data.LoadError = uaFriendlyError(err)
		}
		return
	}

	doors, derr := s.ua.ListDoors(ctx)
	if derr != nil {
		s.log.Warn("device center: list doors failed", "err", derr)
		data.DoorsError = uaFriendlyError(derr)
		doors = nil
	}

	if em, eerr := s.ua.EmergencySettings(ctx); eerr != nil {
		s.log.Warn("device center: emergency status failed", "err", eerr)
	} else if em != nil {
		data.Emergency = buildEmergencyView(em)
	}

	s.buildRows(data, devices, doors, s.viewerMACSet(ctx))
}

// buildRows turns devices + doors into the flat, pre-sorted row list and
// computes the facet counts.
func (s *Server) buildRows(data *uaOverviewData, devices []uaapi.Device, doors []uaapi.Door, mockMACs map[string]bool) {
	catCount := map[string]int{}
	modelCount := map[string]int{}

	for _, d := range devices {
		row := makeDeviceRow(d, mockMACs)
		data.Rows = append(data.Rows, row)
		catCount[row.Category]++
		if row.Model != "" {
			modelCount[row.Model]++
		}
		if d.IsOnline {
			data.OnlineCount++
		} else {
			data.OfflineCount++
		}
	}
	for _, dr := range doors {
		row := makeDoorRow(dr)
		data.Rows = append(data.Rows, row)
		catCount["door"]++
	}

	// Stable order: category rank, then name (case-insensitive).
	sort.SliceStable(data.Rows, func(i, j int) bool {
		ci, cj := categoryOrder[data.Rows[i].Category], categoryOrder[data.Rows[j].Category]
		if ci != cj {
			return ci < cj
		}
		return strings.ToLower(data.Rows[i].Name) < strings.ToLower(data.Rows[j].Name)
	})

	// Number the rows and mark the first of each category as a group
	// start (label + total count) for the table's group headings.
	lastCat := ""
	for i := range data.Rows {
		data.Rows[i].Index = i + 1
		if data.Rows[i].Category != lastCat {
			lastCat = data.Rows[i].Category
			data.Rows[i].GroupStart = true
			data.Rows[i].GroupLabel = categoryPlural(data.Rows[i].Category)
			data.Rows[i].GroupCount = catCount[data.Rows[i].Category]
		}
	}

	data.TotalCount = len(data.Rows)
	data.OnlinePad = pad2(data.OnlineCount)
	data.OfflinePad = pad2(data.OfflineCount)
	data.UpdatesPad = pad2(data.UpdatesCount)

	// Category facet: the real natures present, then the not-yet-wired
	// Cameras/Sensors as disabled shell entries (no invented data).
	for _, c := range []struct{ key, label string }{
		{"hub", "Hubs"}, {"reader", "Readers"}, {"viewer", "Viewers"},
		{"other", "Other devices"}, {"door", "Doors"},
	} {
		if n := catCount[c.key]; n > 0 {
			data.CategoryFacets = append(data.CategoryFacets, uaFacet{Key: c.key, Label: c.label, Count: n})
		}
	}
	data.CategoryFacets = append(data.CategoryFacets,
		uaFacet{Key: "camera", Label: "Cameras", Count: 0, Disabled: true},
		uaFacet{Key: "sensor", Label: "Sensors", Count: 0, Disabled: true},
	)

	// Source facet: only UniFi is real today. Further sources appear
	// here once their device backends exist.
	if data.TotalCount > 0 {
		data.SourceFacets = append(data.SourceFacets, uaFacet{Key: "unifi", Label: "UniFi", Count: data.TotalCount})
	}

	// Status facet: device reachability (doors carry no online/offline).
	// Both facets always render (even at count 0) so the live status
	// poll can move a device between them without a page re-render.
	data.StatusFacets = append(data.StatusFacets,
		uaFacet{Key: "online", Label: "Online", Count: data.OnlineCount},
		uaFacet{Key: "offline", Label: "Offline", Count: data.OfflineCount},
	)

	// Model facet: distinct device models, most common first.
	for m, n := range modelCount {
		data.ModelFacets = append(data.ModelFacets, uaFacet{Key: m, Label: m, Count: n})
	}
	sort.SliceStable(data.ModelFacets, func(i, j int) bool {
		if data.ModelFacets[i].Count != data.ModelFacets[j].Count {
			return data.ModelFacets[i].Count > data.ModelFacets[j].Count
		}
		return data.ModelFacets[i].Label < data.ModelFacets[j].Label
	})
}

// makeDeviceRow builds the flat row for a device, tagging viewers as
// CARVILON mock vs foreign UA by MAC and folding the known fields into
// the panel Detail list.
func makeDeviceRow(d uaapi.Device, mockMACs map[string]bool) uaRow {
	nature := d.Nature()
	row := uaRow{
		ID:          d.ID,
		Kind:        "device",
		Category:    nature,
		TypeLabel:   uaTypeLabel(nature),
		Name:        d.DisplayName(),
		Source:      "unifi",
		SourceLabel: "UniFi",
		Model:       d.ModelLabel(),
		IP:          d.IPLabel(),
		Firmware:    d.FirmwareLabel(),
		Version:     d.VersionLabel(),
		MAC:         d.DisplayMAC(),
		Uptime:      d.UptimeLabel(),
		Capabilities: d.Capabilities,
	}
	if d.IsOnline {
		row.StatusState, row.StatusText = "online", "Online"
	} else {
		row.StatusState, row.StatusText = "offline", "Offline"
	}
	if nature == "viewer" {
		row.Mock = mockMACs[normalizeMACToColonForm(d.ID)]
	}

	det := []kvRow{
		{Key: "Type", Value: row.TypeLabel},
		{Key: "Status", Value: row.StatusText},
		{Key: "Source", Value: row.SourceLabel},
	}
	det = appendKV(det, "Model", row.Model)
	det = appendKV(det, "IP address", row.IP)
	det = appendKV(det, "Firmware", row.Firmware)
	det = appendKV(det, "Device version", row.Version)
	det = appendKV(det, "MAC", row.MAC)
	det = appendKV(det, "Uptime", row.Uptime)
	det = append(det,
		kvRow{Key: "Adopted", Value: uaBoolLabel(d.IsAdopted)},
		kvRow{Key: "Managed", Value: uaBoolLabel(d.IsManaged)},
		kvRow{Key: "Connected", Value: uaBoolLabel(d.IsConnected)},
	)
	det = appendKV(det, "Hub (connected_uah_id)", d.ConnectedUAHID)
	det = appendKV(det, "Location (location_id)", d.LocationID)
	if nature == "viewer" {
		if row.Mock {
			det = appendKV(det, "Viewer origin", "CARVILON mock viewer")
		} else {
			det = appendKV(det, "Viewer origin", "UniFi Access viewer")
		}
	}
	row.Detail = det
	row.Search = strings.ToLower(strings.Join([]string{row.Name, row.Model, row.IP, row.MAC, row.TypeLabel}, " "))
	return row
}

// makeDoorRow builds the flat row for a door.
func makeDoorRow(d uaapi.Door) uaRow {
	row := uaRow{
		ID:          d.ID,
		Kind:        "door",
		Category:    "door",
		TypeLabel:   "Door",
		Name:        d.DisplayName(),
		Source:      "unifi",
		SourceLabel: "UniFi",
		Model:       strings.TrimSpace(d.Type),
	}
	row.LockFromDoor(d)
	row.PositionState = d.PositionState()
	row.Position = uaPositionLabel(d.PositionState(), d.PositionRaw())

	det := []kvRow{
		{Key: "Type", Value: "Door"},
		{Key: "Lock", Value: row.StatusText},
		{Key: "Door position (DPS)", Value: row.Position},
		{Key: "Bound to hub (is_bind_hub)", Value: uaBoundLabel(d)},
	}
	det = appendKV(det, "Model", row.Model)
	det = appendKV(det, "Floor (floor_id)", d.FloorLabel())
	det = appendKV(det, "Hub (hub_id)", d.HubID)
	det = append(det, kvRow{Key: "Door ID", Value: d.ID})
	row.Detail = det
	row.Search = strings.ToLower(strings.Join([]string{row.Name, row.Model, row.TypeLabel}, " "))
	return row
}

// LockFromDoor sets the row's status to the door's lock state.
func (r *uaRow) LockFromDoor(d uaapi.Door) {
	r.StatusState = d.LockState()
	switch r.StatusState {
	case "locked":
		r.StatusText = "Locked"
	case "unlocked":
		r.StatusText = "Unlocked"
	default:
		r.StatusState = "unknown"
		r.StatusText = "Unknown"
	}
}

// pad2 renders a count as a two-digit string for the flip displays,
// clamping to 99 (the display has room for two digits only).
func pad2(n int) string {
	if n < 0 {
		n = 0
	}
	if n > 99 {
		n = 99
	}
	return strconv.Itoa(100 + n)[1:]
}

func appendKV(rows []kvRow, key, val string) []kvRow {
	if strings.TrimSpace(val) == "" {
		return rows
	}
	return append(rows, kvRow{Key: key, Value: val})
}

// categoryPlural is the group-heading label for a category slug.
func categoryPlural(cat string) string {
	switch cat {
	case "hub":
		return "Hubs"
	case "reader":
		return "Readers"
	case "viewer":
		return "Viewers"
	case "door":
		return "Doors"
	default:
		return "Other devices"
	}
}

func uaTypeLabel(nature string) string {
	switch nature {
	case "hub":
		return "Hub"
	case "reader":
		return "Reader"
	case "viewer":
		return "Viewer"
	default:
		return "Device"
	}
}

func uaPositionLabel(state, raw string) string {
	switch state {
	case "open":
		return "Open"
	case "closed":
		return "Closed"
	default:
		if raw != "" {
			return raw
		}
		return "Unknown"
	}
}

func uaBoundLabel(d uaapi.Door) string {
	if v, ok := d.BoundToHub(); ok {
		if v {
			return "Yes"
		}
		return "No"
	}
	return "Unknown"
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
		s.log.Warn("device center: viewer mac lookup failed", "err", err)
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

// uaStatusItem is one row's live status in the /a/ua/status payload,
// addressed by kind+id (matching the row's data attributes).
type uaStatusItem struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Status string `json:"status"`
	Text   string `json:"text"`
	Pos    string `json:"pos,omitempty"`
}

// handleAdminUAStatus serves a lightweight live snapshot of every
// row's status plus the fleet counters as JSON. The Device Center
// polls it so an online/offline (or lock-state) change shows up
// without a manual reload. It uses the UDM's cached device list (no
// refresh=true): this runs every few seconds and must stay cheap.
func (s *Server) handleAdminUAStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if !s.uaReady(r.Context()) {
		writeUADetailError(w, "UniFi Access is not active or not configured.")
		return
	}
	devices, err := s.ua.ListDevices(r.Context())
	if err != nil {
		// Friendly string only; the client keeps its last state and
		// simply retries on the next poll.
		writeUADetailError(w, uaFriendlyError(err))
		return
	}
	items := make([]uaStatusItem, 0, len(devices))
	online, offline := 0, 0
	for _, d := range devices {
		st, txt := "offline", "Offline"
		if d.IsOnline {
			st, txt = "online", "Online"
			online++
		} else {
			offline++
		}
		items = append(items, uaStatusItem{Kind: "device", ID: d.ID, Status: st, Text: txt})
	}
	counts := map[string]any{"online": online, "offline": offline, "updates": 0}
	// Doors are secondary here: a doors failure drops the door items
	// and the total (the client then leaves both untouched) but never
	// blocks the device statuses.
	if doors, derr := s.ua.ListDoors(r.Context()); derr == nil {
		for _, dr := range doors {
			var row uaRow
			row.LockFromDoor(dr)
			items = append(items, uaStatusItem{
				Kind: "door", ID: dr.ID, Status: row.StatusState, Text: row.StatusText,
				Pos: uaPositionLabel(dr.PositionState(), dr.PositionRaw()),
			})
		}
		counts["total"] = len(devices) + len(doors)
	} else {
		s.log.Warn("device center: status poll doors failed", "err", derr)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "counts": counts, "items": items})
}

// handleAdminUADeviceSettings lazily serves the /devices/:id/settings
// detail for one device as JSON, loaded when its panel opens.
func (s *Server) handleAdminUADeviceSettings(w http.ResponseWriter, r *http.Request) {
	id, ok := s.uaDetailPrelude(w, r)
	if !ok {
		return
	}
	sec := uaSection{Title: "Access methods"}
	if v, err := s.ua.DeviceSettings(r.Context(), id); err != nil {
		s.log.Warn("device center: device settings failed", "err", err)
		sec.Error = uaFriendlyError(err)
	} else {
		sec.Rows = flattenUADetail(v)
	}
	writeUADetail(w, sec)
}

// handleAdminUADoorDetail lazily serves the full door record plus its
// lock rule as JSON, loaded when a door panel opens.
func (s *Server) handleAdminUADoorDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := s.uaDetailPrelude(w, r)
	if !ok {
		return
	}
	detail := uaSection{Title: "Door details"}
	if v, err := s.ua.DoorDetail(r.Context(), id); err != nil {
		s.log.Warn("device center: door detail failed", "err", err)
		detail.Error = uaFriendlyError(err)
	} else {
		detail.Rows = flattenUADetail(v)
	}

	rule := uaSection{Title: "Lock rule"}
	if v, err := s.ua.DoorLockRule(r.Context(), id); err != nil {
		// A door with no rule set answers not-found; that is a clean
		// "no rule", not an error.
		if !errors.Is(err, uaapi.ErrNotFound) {
			s.log.Warn("device center: door lock rule failed", "err", err)
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
		writeUADetailError(w, "UniFi Access is not active or not configured.")
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

// uaFriendlyError maps a uaapi error to a fixed English message. It
// never embeds the underlying error text, which can carry the UDM host
// - the token/host must never reach the HTML or JSON.
func uaFriendlyError(err error) string {
	switch {
	case errors.Is(err, uaapi.ErrUnauthorized):
		return "Access denied - please check the UA API token (401)."
	case errors.Is(err, uaapi.ErrNotFound):
		return "Not found."
	default:
		return "UDM unreachable or the response was invalid."
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
		*out = append(*out, kvRow{Key: prefix, Value: "-"})
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
		return "Yes"
	}
	return "No"
}
