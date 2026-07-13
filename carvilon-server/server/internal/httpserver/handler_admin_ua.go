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
// so those categories render empty/disabled - no invented data.
//
// Besides the two UniFi integrations the page carries a third,
// independent source: CARVILON's own tag readers (the local registry,
// migrations 036/037) appear as source "RPi" in the same flat table.
// They are OUR data, so their rows keep their two controls (rename,
// editor jump) in the detail panel while everything UniFi stays
// read-only.
//
// Shelly Etappe 1 adds the fourth source: the admin-configured Shelly
// devices (Gen2+ local RPC, category "switch", source "Shelly") -
// read-only rows whose panel lazily fetches the live channel
// measurements. See handler_admin_shelly.go.
//
// Gating: the page only talks to the UDM when the "UA aktiv" toggle is
// on AND a client is configured (base URL + token). Everything else is
// a clean hint - but the RPi source is NOT behind that gate: with a
// reader registered the table always renders and UA trouble degrades
// to a banner. The token/host never reach the log or the HTML - only
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
	"time"

	"carvilon.local/server/internal/protectapi"
	"carvilon.local/server/internal/readerstore"
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

	// Protect Etappe 1: whether the UniFi Protect integration is on
	// AND wired. When true the page always renders the table (never a
	// UA gate card) and UA trouble degrades to the UANotice banner.
	ProtectAvailable bool

	// LocalAvailable: at least one CARVILON reader is registered
	// (source "RPi"). Like ProtectAvailable it keeps the table on
	// screen and degrades UA trouble to a banner - the local source
	// is independent of any UniFi configuration.
	LocalAvailable bool

	// ShellyAvailable: the Shelly integration is on AND device
	// addresses are configured (Shelly Etappe 1). Independent of every
	// UniFi integration, same page-keeping role as the other sources.
	ShellyAvailable bool

	// ShellyEnabled: the Shelly integration toggle is on (discovery runs),
	// regardless of whether any device is adopted yet. Gates the discovery
	// actions in the toolbar (Scan Shelly / Scan network) so they are
	// reachable even before the first device lands.
	ShellyEnabled bool

	// ShellyDiscovery keeps the table (not the gate card) on screen whenever
	// Shelly discovery is relevant: the integration is enabled, OR there are
	// pending/ignored devices to show. So a discovered device appears even
	// before anything is adopted and with every UniFi integration off.
	ShellyDiscovery bool

	// MideaEnabled: the Midea Climate Controller source is wired (store present).
	// Gates the local-discovery action in the toolbar so it is reachable even
	// before the first device is adopted. E1 has no separate on/off toggle - the
	// approval gate keeps discovery safe out of the box.
	MideaEnabled bool

	// Flash is the outcome banner after a reader rename (the page's
	// only write). Set from a stable code in the redirect query -
	// never from free text. FlashType is "ok" or "err".
	Flash     string
	FlashType string

	// Terminal error states (devices could not be listed at all AND
	// Protect cannot fill the page either) -> full-page gate cards.
	Unauthorized bool   // 401 from the UDM -> token hint
	LoadError    string // any other devices-fetch failure -> unreachable hint

	// Section-level, non-fatal banners.
	DoorsError   string // devices loaded but doors did not
	UANotice     string // UA off/unconfigured/failed while Protect still fills the page
	ProtectError string // cameras/sensors could not be loaded (page keeps the UA rows)

	Emergency *uaEmergencyView

	Rows []uaRow

	CategoryFacets  []uaFacet
	SourceFacets    []uaFacet
	StatusFacets    []uaFacet
	ModelFacets     []uaFacet
	LifecycleFacets []uaFacet

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

	Source      string // "unifi" | "rpi"
	SourceLabel string // "UniFi" | "RPi"

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

	// RPi-reader extras: the rename form in the detail panel prefills
	// the custom name and shows the auto-name as its placeholder.
	AutoName   string
	CustomName string

	// Lifecycle is the discovery axis, orthogonal to Category: "" / "adopted"
	// for the normal active set, "pending" for a discovered-but-not-adopted
	// Shelly (pinned to the top group), "ignored" for a sticky-removed one
	// (pinned to the bottom group). Drives the lifecycle facet + the row's
	// inline approve/reject/release actions.
	Lifecycle string

	// Shelly extras: how the device entered the set ("manual" |
	// "discovered"), for the panel's origin line + the sticky Remove
	// control, and the MQTT provisioning state. Empty for non-Shelly rows.
	Origin    string
	MQTTState string

	// Shelly cockpit plumbing (empty for non-Shelly rows): the store id
	// keys the designer HTTP-RPC config/schedule endpoints, the prefix
	// ("carvilon/<broker-user>" for Gen2+, "shellies/<broker-user>" for
	// Gen1) addresses the live status topics and the relay-command
	// publish, ChannelsJSON is the capability-derived channel set
	// ([{"id":0,"meter":true},...]) the function area renders its
	// per-channel cards from, and ShellyGen tells the cockpit which
	// topic/payload grammar the device speaks (0 = not yet classified).
	ShellyID     int64
	ShellyGen    int
	ShellyPrefix string
	ChannelsJSON string

	// Midea Climate Controller cockpit plumbing (empty for non-Midea rows):
	// the current mode/fan/setpoint prefill the standard-profile control forms,
	// MideaProfile ("standard" | "advanced") drives the profile toggle. The
	// store id (uaRow.ID) keys the control/approve/export endpoints.
	MideaMode     string
	MideaFan      string
	MideaSetpoint string
	MideaProfile  string

	// Sensor History H1: the per-sensor recording knobs prefilled into the
	// cockpit form. RecIntervalSec/RecRetentionSec are the stored OVERRIDE
	// seconds (0 = inherit the global default); the Default*Label pair names
	// what "Default" resolves to so the form can show it. Empty for non-sensors.
	RecIntervalSec           int64
	RecRetentionSec          int64
	RecDefaultIntervalLabel  string
	RecDefaultRetentionLabel string

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
var categoryOrder = map[string]int{
	// "pending" and "ignored" are lifecycle pseudo-categories: their ranks
	// pin the discovery groups to the very top / very bottom of the table,
	// with the real device categories in between (existing within-group sort
	// preserved). See uaRow.Lifecycle.
	"pending": -2,
	"hub":     0, "reader": 1, "viewer": 2, "camera": 3, "sensor": 4, "switch": 5, "rgbw": 6, "midea-climate": 7, "other": 8, "door": 9,
	"ignored": 99,
}

// handleAdminUA renders the Device Center (read-only except for the
// local readers' rename control).
func (s *Server) handleAdminUA(w http.ResponseWriter, r *http.Request) {
	username := AdminUserFromContext(r.Context())
	data := uaOverviewData{
		User:             adminUser{Name: username, Initials: initialsOf(username)},
		Enabled:          s.uaEnabled(r.Context()),
		ProtectAvailable: s.protectReady(r.Context()),
		ShellyAvailable:  s.shellyReady(r.Context()),
		ShellyEnabled:    s.shellyEnabled(r.Context()),
	}
	data.Configured = data.Enabled && s.ua != nil
	if f, ok := uaFlash[r.URL.Query().Get("flash")]; ok {
		data.Flash, data.FlashType = f.msg, f.typ
	}
	readers := s.localReaders(r.Context())
	data.LocalAvailable = len(readers) > 0
	// Discovered-but-not-adopted (pending) + ignored Shelly devices surface
	// as rows in the table (pinned top / bottom). Fetched here so their mere
	// presence keeps the page rendered even before anything is adopted.
	pendingRows, ignoredRows := s.shellyLifecycleRows(r.Context())
	data.ShellyDiscovery = data.ShellyEnabled || len(pendingRows)+len(ignoredRows) > 0
	// Midea Climate Controller (Etappe 1): active devices become their own
	// source rows; pending/ignored fold into the shared lifecycle groups.
	mideaActive, mideaPending, mideaIgnored := s.mideaLifecycleRows(r.Context())
	pendingRows = append(pendingRows, mideaPending...)
	ignoredRows = append(ignoredRows, mideaIgnored...)
	data.MideaEnabled = s.mideaReady()
	mideaHasRows := len(mideaActive)+len(mideaPending)+len(mideaIgnored) > 0
	// No source can fill the page -> no calls at all, just the gate
	// hints (Etappe-1 contract, now covering all sources + discovery).
	if !(data.Enabled && data.Configured) && !data.ProtectAvailable && !data.LocalAvailable && !data.ShellyDiscovery && !data.MideaEnabled && !mideaHasRows {
		s.renderAdminPage(w, "ua", data)
		return
	}
	s.buildUAOverview(r.Context(), &data, readers, mideaActive, pendingRows, ignoredRows)
	s.renderAdminPage(w, "ua", data)
}

// buildUAOverview fetches UA devices (with ?refresh=true), doors and
// the emergency status plus - Protect Etappe 1 - the Protect cameras
// and sensors, folds in the local reader registry (source "RPi"), and
// flattens everything into rows + facets. Each fetch is isolated: a
// doors failure does not blank the (already loaded) devices, a Protect
// failure only drops a banner, and with another source available a UA
// failure degrades to a banner instead of a gate.
func (s *Server) buildUAOverview(ctx context.Context, data *uaOverviewData, readers []readerstore.Reader, mideaActive, pendingRows, ignoredRows []uaRow) {
	// The Shelly probe fans out over the configured devices with its
	// own per-device timeout; run it alongside the UniFi fetches so a
	// dead box delays the page by max(sources), not their sum. The
	// channel is buffered - the goroutine can never leak.
	shellyCh := make(chan []shellyProbe, 1)
	if data.ShellyAvailable {
		go func() { shellyCh <- s.probeShelly(ctx) }()
	} else {
		shellyCh <- nil
	}

	var devices []uaapi.Device
	var doors []uaapi.Door
	switch {
	case data.Enabled && data.Configured:
		var err error
		devices, err = s.ua.ListDevicesRefresh(ctx)
		if err != nil {
			if !data.ProtectAvailable && !data.LocalAvailable && !data.ShellyAvailable {
				// Terminal, as before other sources existed: gate card.
				if errors.Is(err, uaapi.ErrUnauthorized) {
					data.Unauthorized = true
				} else {
					s.log.Warn("device center: list devices failed", "err", err)
					data.LoadError = uaFriendlyError(err)
				}
				return
			}
			s.log.Warn("device center: list devices failed", "err", err)
			data.UANotice = "UniFi Access devices could not be loaded. " + uaFriendlyError(err)
			devices = nil
		} else {
			var derr error
			doors, derr = s.ua.ListDoors(ctx)
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
		}
	default:
		// UA off/unconfigured but another source fills the page.
		data.UANotice = "UniFi Access is disabled or not configured - only devices from other sources are shown."
	}

	var cams []protectapi.Camera
	var sens []protectapi.Sensor
	if data.ProtectAvailable {
		var cerr, serr error
		cams, cerr = s.protect.ListCameras(ctx)
		sens, serr = s.protect.ListSensors(ctx)
		if cerr != nil {
			s.log.Warn("device center: list cameras failed", "err", cerr)
			cams = nil
		}
		if serr != nil {
			s.log.Warn("device center: list sensors failed", "err", serr)
			sens = nil
		}
		if cerr != nil || serr != nil {
			ferr := cerr
			if ferr == nil {
				ferr = serr
			}
			data.ProtectError = protectFriendlyError(ferr)
		}
	}

	s.buildRows(data, devices, doors, cams, sens, readers, <-shellyCh, s.shellyRowInfoByAddr(ctx), s.viewerMACSet(ctx), mideaActive, pendingRows, ignoredRows)
}

// shellyRowInfo carries the store-side facts a Shelly row shows beyond the
// live probe: how the device entered the set and its MQTT provisioning
// state. Kept local to httpserver so buildRows/makeShellyRow need no store
// import.
type shellyRowInfo struct {
	Origin    string // "manual" | "discovered"
	MQTTState string // "" | "provisioning" | "linked" | "failed"

	// Device-cockpit plumbing: the store id (the shelly HTTP-RPC
	// config/schedule endpoints key by it), the broker identity for the
	// topic prefix, and the store-side MAC/model for the prefix fallback
	// + the capability-derived channel set when the live probe is down.
	StoreID      int64
	MQTTUsername string // provisioned broker account ("" until provisioned)
	MAC          string // normalised uppercase hex ("" when unknown)
	Model        string // last-seen model ("" when unknown)
	Name         string // last-seen display name (mDNS instance label; "" when unknown)
	Gen          int    // stored API generation (0 until classified)
}

// shellyRowInfoByAddr maps active-device address -> its store-side info, so
// a row can show origin + MQTT link state. Empty (nil-safe) on any store
// trouble - the row then omits both.
func (s *Server) shellyRowInfoByAddr(ctx context.Context) map[string]shellyRowInfo {
	m := map[string]shellyRowInfo{}
	if s.shellystore == nil {
		return m
	}
	active, err := s.shellystore.ListActive(ctx)
	if err != nil {
		return m
	}
	for _, d := range active {
		m[d.Address] = shellyRowInfo{
			Origin: d.Origin, MQTTState: d.MQTTState,
			StoreID: d.ID, MQTTUsername: d.MQTTUsername, MAC: d.MAC, Model: d.Model,
			Name: d.Name, Gen: d.Gen,
		}
	}
	return m
}

// buildRows turns devices + doors + Protect cameras/sensors + local
// readers + Shelly devices into the flat, pre-sorted row list and
// computes the facet counts.
func (s *Server) buildRows(data *uaOverviewData, devices []uaapi.Device, doors []uaapi.Door, cams []protectapi.Camera, sens []protectapi.Sensor, readers []readerstore.Reader, shellies []shellyProbe, shellyInfo map[string]shellyRowInfo, mockMACs map[string]bool, mideaActive, pendingRows, ignoredRows []uaRow) {
	catCount := map[string]int{}
	modelCount := map[string]int{}

	// addDeviceRow folds one online/offline-style row into the list
	// and every counter (doors have their own path: lock state, no
	// online/offline contribution).
	addDeviceRow := func(row uaRow, online bool) {
		data.Rows = append(data.Rows, row)
		catCount[row.Category]++
		if row.Model != "" {
			modelCount[row.Model]++
		}
		if online {
			data.OnlineCount++
		} else {
			data.OfflineCount++
		}
	}

	for _, d := range devices {
		addDeviceRow(makeDeviceRow(d, mockMACs), d.IsOnline)
	}
	now := time.Now()
	for _, c := range cams {
		addDeviceRow(makeCameraRow(c), c.IsOnline())
	}
	for _, sn := range sens {
		row := makeSensorRow(sn, now)
		s.fillSensorRecording(&row)
		addDeviceRow(row, sn.IsOnline())
	}
	for _, rd := range readers {
		addDeviceRow(makeReaderRow(rd), rd.Online)
	}
	for _, p := range shellies {
		addDeviceRow(makeShellyRow(p, shellyInfo[p.client.Address()]), p.err == nil)
	}
	// Midea Climate Controllers (adopted): their live status comes from the
	// monitor snapshot the row already carries, so they fold in like any other
	// online/offline source.
	for _, row := range mideaActive {
		addDeviceRow(row, row.StatusState == "online")
	}
	for _, dr := range doors {
		row := makeDoorRow(dr)
		data.Rows = append(data.Rows, row)
		catCount["door"]++
	}
	// Discovered-but-not-adopted (pending) + sticky-removed (ignored) Shelly
	// devices. Like doors they carry no online/offline contribution (never
	// probed); their category ranks (pending -2, ignored 99) pin them to the
	// top / bottom group. Counted for their group heading only - the Category
	// facet stays the real natures; the Lifecycle facet filters these.
	for _, row := range pendingRows {
		data.Rows = append(data.Rows, row)
		catCount["pending"]++
	}
	for _, row := range ignoredRows {
		data.Rows = append(data.Rows, row)
		catCount["ignored"]++
	}

	// Stable order: category rank, then name (case-insensitive).
	sort.SliceStable(data.Rows, func(i, j int) bool {
		ci, cj := categoryOrder[data.Rows[i].Category], categoryOrder[data.Rows[j].Category]
		if ci != cj {
			return ci < cj
		}
		return strings.ToLower(data.Rows[i].Name) < strings.ToLower(data.Rows[j].Name)
	})

	// Number the rows, mark the first of each category as a group
	// start (label + total count) for the table's group headings, and
	// count the sources for their facet.
	lastCat := ""
	srcCount := map[string]int{}
	lifeCount := map[string]int{}
	for i := range data.Rows {
		data.Rows[i].Index = i + 1
		srcCount[data.Rows[i].Source]++
		// Every normal (active) row is "adopted" for the lifecycle facet;
		// pending/ignored rows set their own value in the row builder.
		if data.Rows[i].Lifecycle == "" {
			data.Rows[i].Lifecycle = "adopted"
		}
		lifeCount[data.Rows[i].Lifecycle]++
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

	// Category facet: the real natures present. Cameras/Sensors are
	// real (enabled, live counts) once the Protect integration is
	// available; without it they stay the disabled shell entries of
	// Etappe 1 - no invented data either way.
	for _, c := range []struct{ key, label string }{
		{"hub", "Hubs"}, {"reader", "Readers"}, {"viewer", "Viewers"},
		{"camera", "Cameras"}, {"sensor", "Sensors"}, {"switch", "Switches"},
		{"rgbw", "RGBW Dimmers"}, {"midea-climate", "Midea Climate Controllers"},
		{"other", "Other devices"}, {"door", "Doors"},
	} {
		n := catCount[c.key]
		switch c.key {
		case "camera", "sensor":
			data.CategoryFacets = append(data.CategoryFacets,
				uaFacet{Key: c.key, Label: c.label, Count: n, Disabled: !data.ProtectAvailable})
		default:
			if n > 0 {
				data.CategoryFacets = append(data.CategoryFacets, uaFacet{Key: c.key, Label: c.label, Count: n})
			}
		}
	}

	// Source facet: every source with at least one row. UniFi covers
	// the UA + Protect rows, RPi the local reader registry, Shelly
	// the configured Shelly devices.
	for _, sc := range []struct{ key, label string }{
		{"unifi", "UniFi"}, {"rpi", "RPi"}, {"shelly", "Shelly"}, {"midea", "Midea"},
	} {
		if n := srcCount[sc.key]; n > 0 {
			data.SourceFacets = append(data.SourceFacets, uaFacet{Key: sc.key, Label: sc.label, Count: n})
		}
	}

	// Status facet: device reachability (doors carry no online/offline).
	// Both facets always render (even at count 0) so the live status
	// poll can move a device between them without a page re-render.
	data.StatusFacets = append(data.StatusFacets,
		uaFacet{Key: "online", Label: "Online", Count: data.OnlineCount},
		uaFacet{Key: "offline", Label: "Offline", Count: data.OfflineCount},
	)

	// Lifecycle facet: the discovery axis. "Adopted" (the normal active set)
	// always renders; Pending / Ignored only when present, so a clean fleet
	// shows just Adopted.
	data.LifecycleFacets = append(data.LifecycleFacets,
		uaFacet{Key: "adopted", Label: "Adopted", Count: lifeCount["adopted"]})
	if n := lifeCount["pending"]; n > 0 {
		data.LifecycleFacets = append(data.LifecycleFacets,
			uaFacet{Key: "pending", Label: "Pending", Count: n})
	}
	if n := lifeCount["ignored"]; n > 0 {
		data.LifecycleFacets = append(data.LifecycleFacets,
			uaFacet{Key: "ignored", Label: "Ignored", Count: n})
	}

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
		ID:           d.ID,
		Kind:         "device",
		Category:     nature,
		TypeLabel:    uaTypeLabel(nature),
		Name:         d.DisplayName(),
		Source:       "unifi",
		SourceLabel:  "UniFi",
		Model:        d.ModelLabel(),
		IP:           d.IPLabel(),
		Firmware:     d.FirmwareLabel(),
		Version:      d.VersionLabel(),
		MAC:          d.DisplayMAC(),
		Uptime:       d.UptimeLabel(),
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

// makeCameraRow builds the flat row for a Protect camera. Only name,
// state and MAC are reliably present in the Integration API; model,
// IP and firmware degrade to "-" via their empty labels - nothing is
// invented (Protect Etappe 1 contract).
func makeCameraRow(c protectapi.Camera) uaRow {
	row := uaRow{
		ID:          c.ID,
		Kind:        "camera",
		Category:    "camera",
		TypeLabel:   "Camera",
		Name:        c.DisplayName(),
		Source:      "unifi",
		SourceLabel: "UniFi",
		Model:       c.ModelLabel(),
		IP:          c.IPLabel(),
		Firmware:    c.FirmwareLabel(),
		MAC:         c.MACLabel(),
	}
	if c.IsOnline() {
		row.StatusState, row.StatusText = "online", "Online"
	} else {
		row.StatusState, row.StatusText = "offline", "Offline"
	}

	det := []kvRow{
		{Key: "Type", Value: "Camera"},
		{Key: "Status", Value: row.StatusText},
		{Key: "Source", Value: row.SourceLabel},
	}
	det = appendKVDash(det, "Model", row.Model)
	det = appendKVDash(det, "IP address", row.IP)
	det = appendKVDash(det, "Firmware", row.Firmware)
	det = appendKVDash(det, "MAC", row.MAC)
	det = appendKVDash(det, "Video mode", c.VideoModeLabel())
	det = appendKVDash(det, "HDR type", c.HDRTypeLabel())
	det = appendKVDash(det, "Package camera", c.PackageCameraLabel())
	det = append(det, kvRow{Key: "Camera ID", Value: c.ID})
	row.Detail = det
	row.Search = strings.ToLower(strings.Join([]string{row.Name, row.Model, row.IP, row.MAC, row.TypeLabel}, " "))
	return row
}

// makeSensorRow builds the flat row for a Protect sensor. The
// measurements live in the panel's Overview block (the shared table
// columns stay identical across every category, like the doors do);
// absent readings render "-" instead of invented values.
func makeSensorRow(sn protectapi.Sensor, now time.Time) uaRow {
	row := uaRow{
		ID:          sn.ID,
		Kind:        "sensor",
		Category:    "sensor",
		TypeLabel:   "Sensor",
		Name:        sn.DisplayName(),
		Source:      "unifi",
		SourceLabel: "UniFi",
		Model:       sn.ModelLabel(),
		MAC:         sn.MACLabel(),
	}
	if sn.IsOnline() {
		row.StatusState, row.StatusText = "online", "Online"
	} else {
		row.StatusState, row.StatusText = "offline", "Offline"
	}

	det := []kvRow{
		{Key: "Type", Value: "Sensor"},
		{Key: "Status", Value: row.StatusText},
		{Key: "Source", Value: row.SourceLabel},
	}
	det = appendKVDash(det, "Model", row.Model)
	det = appendKVDash(det, "MAC", row.MAC)
	det = appendKVDash(det, "Temperature", sn.TemperatureLabel())
	det = appendKVDash(det, "Humidity", sn.HumidityLabel())
	det = appendKVDash(det, "Light", sn.LightLabel())
	det = appendKVDash(det, "Motion", sn.MotionLabel())
	det = appendKVDash(det, "Water leak", sn.LeakLabel(now))
	det = appendKVDash(det, "Tamper", sn.TamperLabel(now))
	det = appendKVDash(det, "Signal", sn.SignalLabel())
	det = appendKVDash(det, "Battery", sn.BatteryLabel())
	det = appendKVDash(det, "Connected to", sn.BridgeLabel())
	det = appendKVDash(det, "Mount type", sn.MountTypeLabel())
	det = appendKVDash(det, "Opened", sn.OpenedLabel())
	det = append(det, kvRow{Key: "Sensor ID", Value: sn.ID})
	row.Detail = det
	row.Search = strings.ToLower(strings.Join([]string{row.Name, row.Model, row.MAC, row.TypeLabel}, " "))
	return row
}

// fillSensorRecording prefills a sensor row's recording-settings fields
// (Sensor History H1) from the per-sensor config: the stored override
// seconds (0 = inherit) so the form selects the right option, plus the
// human labels of what "Default" resolves to. A nil config store (history
// disabled) leaves the zero values and the form shows only its defaults.
func (s *Server) fillSensorRecording(row *uaRow) {
	if s.sensorHistCfg == nil {
		return
	}
	row.RecIntervalSec, row.RecRetentionSec = s.sensorHistCfg.Raw(row.ID)
	def := s.sensorHistCfg.Defaults()
	row.RecDefaultIntervalLabel = recordingIntervalLabel(int64(def.Interval.Seconds()))
	row.RecDefaultRetentionLabel = recordingRetentionLabel(int64(def.Retention.Seconds()))
}

// recordingIntervalLabel renders an interval (seconds) as a compact label
// ("30 s", "1 min", "1 h"); "" when unset.
func recordingIntervalLabel(sec int64) string {
	switch {
	case sec <= 0:
		return ""
	case sec%3600 == 0:
		return strconv.FormatInt(sec/3600, 10) + " h"
	case sec%60 == 0:
		return strconv.FormatInt(sec/60, 10) + " min"
	default:
		return strconv.FormatInt(sec, 10) + " s"
	}
}

// recordingRetentionLabel renders a retention age (seconds) as days ("30
// days"), or hours below a day; "" when unset.
func recordingRetentionLabel(sec int64) string {
	switch {
	case sec <= 0:
		return ""
	case sec >= 86400:
		return strconv.FormatInt(sec/86400, 10) + " days"
	default:
		return strconv.FormatInt(sec/3600, 10) + " h"
	}
}

// makeReaderRow builds the flat row for a CARVILON reader from the
// local registry (source "RPi"). It shares the Readers category with
// the UA readers - the Source facet is what tells them apart. All the
// registry knows is already in the row, so the panel needs no lazy
// fetch; its right column carries the reader's controls instead
// (rename + editor jump). Columns the registry has no data for (IP,
// device version, MAC) stay empty and render "-".
func makeReaderRow(rd readerstore.Reader) uaRow {
	row := uaRow{
		ID:          rd.ID,
		Kind:        "rpi-reader",
		Category:    "reader",
		TypeLabel:   "Reader",
		Name:        rd.DisplayName(),
		Source:      "rpi",
		SourceLabel: "RPi",
		Model:       rd.Model,
		Firmware:    rd.Firmware,
		AutoName:    rd.Name,
		CustomName:  rd.CustomName,
	}
	if rd.Online {
		row.StatusState, row.StatusText = "online", "Online"
	} else {
		row.StatusState, row.StatusText = "offline", "Offline"
	}

	det := []kvRow{
		{Key: "Type", Value: "Reader"},
		{Key: "Status", Value: row.StatusText},
		{Key: "Source", Value: row.SourceLabel},
	}
	det = appendKVDash(det, "Model", rd.Model)
	det = appendKVDash(det, "Firmware", rd.Firmware)
	det = appendKVDash(det, "Bus", rd.Bus)
	det = append(det, kvRow{Key: "Identity", Value: rd.ID})
	if rd.CustomName != "" {
		det = append(det, kvRow{Key: "Auto name", Value: rd.Name})
	}
	det = appendKVDash(det, "Last tag", rd.LastUID)
	det = appendKVDash(det, "Last tag seen", readerLastSeenLabel(rd))
	row.Detail = det
	row.Search = strings.ToLower(strings.Join([]string{row.Name, row.Model, rd.Bus, rd.ID, row.TypeLabel, "rpi"}, " "))
	return row
}

// readerLastSeenLabel formats a reader's last tag-read time, "" if the
// reader never saw a tag.
func readerLastSeenLabel(rd readerstore.Reader) string {
	if rd.LastSeenAt <= 0 {
		return ""
	}
	return time.UnixMilli(rd.LastSeenAt).Format("2006-01-02 15:04:05")
}

// localReaders lists the CARVILON reader registry. Nil-safe (no store
// wired) and non-fatal on error - the Device Center then simply shows
// no local rows.
func (s *Server) localReaders(ctx context.Context) []readerstore.Reader {
	if s.readerStore == nil {
		return nil
	}
	readers, err := s.readerStore.List(ctx)
	if err != nil {
		s.log.Warn("device center: list readers failed", "err", err)
		return nil
	}
	return readers
}

// uaFlash maps a stable flash code (carried in the redirect query,
// never free text) to the banner after a reader rename.
var uaFlash = map[string]struct{ msg, typ string }{
	"renamed":               {"Reader name saved.", "ok"},
	"reset":                 {"Reader name reset to the auto-generated name.", "ok"},
	"err-name":              {"Renaming failed.", "err"},
	"rec-saved":             {"Recording settings saved.", "ok"},
	"rec-err":               {"Saving the recording settings failed.", "err"},
	"err-notfd":             {"Reader not found.", "err"},
	"shelly-removed":        {"Shelly device removed. It will not be re-discovered until released.", "ok"},
	"shelly-notfd":          {"Shelly device not found.", "err"},
	"shelly-err":            {"The Shelly device action failed.", "err"},
	"shelly-provisioning":   {"Provisioning the Shelly onto the MQTT broker - this can take a moment.", "ok"},
	"shelly-noprov":         {"The MQTT broker is not running - start it under Settings before provisioning.", "err"},
	"shelly-approved":       {"Shelly device approved - provisioning it onto the MQTT broker.", "ok"},
	"shelly-ignored":        {"Shelly device moved to Ignored. Release it to allow re-discovery.", "ok"},
	"shelly-released":       {"Shelly device released - discovery can find it again.", "ok"},
	"shelly-cap":            {"Active Shelly device limit reached - remove one before approving another.", "err"},
	"midea-scan-ok":         {"Discovery finished - new Midea devices appear in the Pending group.", "ok"},
	"midea-scan-none":       {"No Midea devices answered discovery. Try a targeted IP if they are on another subnet.", "ok"},
	"midea-scan-err":        {"Midea discovery failed.", "err"},
	"midea-approved":        {"Midea device adopted - it is being connected now.", "ok"},
	"midea-pair-err":        {"Adoption failed: could not obtain or verify credentials. Try again, choose the right region, or paste exported keys.", "err"},
	"midea-import-bad":      {"The pasted credentials could not be read - expected the exported key file format.", "err"},
	"midea-ignored":         {"Midea device moved to Ignored. Release it to allow re-discovery.", "ok"},
	"midea-released":        {"Midea device released - discovery can find it again.", "ok"},
	"midea-removed":         {"Midea device removed and its stored credentials dropped.", "ok"},
	"midea-sent":            {"Command sent to the Midea device.", "ok"},
	"midea-ctrl-err":        {"The Midea device did not accept the command.", "err"},
	"midea-badval":          {"That value is out of range.", "err"},
	"midea-profile":         {"Profile saved.", "ok"},
	"midea-advanced-locked": {"The advanced profile (server-side control loop) is not available yet - it lands in a later update.", "err"},
	"midea-notfd":           {"Midea device not found.", "err"},
	"midea-err":             {"The Midea device action failed.", "err"},
}

// handleAdminUAReaderRename sets or clears a local reader's custom name
// from the Device Center panel - the page's only write, and it only
// touches CARVILON's own registry, never UA. Clearing (empty name)
// reverts to the speaking auto-name. Redirects back with a stable flash
// code - never reflects the submitted text.
func (s *Server) handleAdminUAReaderRename(w http.ResponseWriter, r *http.Request) {
	if s.readerStore == nil {
		http.Redirect(w, r, "/a/devices", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/a/devices?flash=err-name", http.StatusSeeOther)
		return
	}
	id := strings.TrimSpace(r.PostFormValue("id"))
	name := strings.TrimSpace(r.PostFormValue("name"))
	// The form's maxlength is 80; that is client-side only, so a direct
	// POST beyond it is truncated here rather than rejected.
	if rn := []rune(name); len(rn) > 80 {
		name = string(rn[:80])
	}
	if id == "" {
		http.Redirect(w, r, "/a/devices?flash=err-name", http.StatusSeeOther)
		return
	}
	err := s.readerStore.SetCustomName(r.Context(), id, name)
	switch {
	case errors.Is(err, readerstore.ErrNotFound):
		http.Redirect(w, r, "/a/devices?flash=err-notfd", http.StatusSeeOther)
	case err != nil:
		s.log.Error("device center: set reader custom name", "reader", id, "err", err)
		http.Redirect(w, r, "/a/devices?flash=err-name", http.StatusSeeOther)
	case name == "":
		http.Redirect(w, r, "/a/devices?flash=reset", http.StatusSeeOther)
	default:
		http.Redirect(w, r, "/a/devices?flash=renamed", http.StatusSeeOther)
	}
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

// appendKVDash always appends the line, degrading an absent value to
// "-" - the Protect rows show every briefed field honestly instead of
// hiding what the NVR did not send.
func appendKVDash(rows []kvRow, key, val string) []kvRow {
	if strings.TrimSpace(val) == "" {
		val = "-"
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
	case "camera":
		return "Cameras"
	case "sensor":
		return "Sensors"
	case "switch":
		return "Switches"
	case "rgbw":
		return "RGBW Dimmers"
	case "midea-climate":
		return "Midea Climate Controllers"
	case "door":
		return "Doors"
	case "pending":
		return "Pending approval"
	case "ignored":
		return "Ignored"
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

func (s *Server) protectReady(ctx context.Context) bool {
	return s.protectEnabled(ctx) && s.protect != nil
}

// protectFriendlyError maps a protectapi error to a fixed English
// message. Like uaFriendlyError it never embeds the underlying error
// text - the host/key must never reach the HTML or JSON.
func protectFriendlyError(err error) string {
	switch {
	case errors.Is(err, protectapi.ErrUnauthorized):
		return "Access denied - please check the Protect API key (401)."
	case errors.Is(err, protectapi.ErrNotFound):
		return "Not found."
	default:
		return "Protect API unreachable or the response was invalid."
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

// uaStatusItem is one row's live status in the /a/devices/status payload,
// addressed by kind+id (matching the row's data attributes). Local
// readers additionally carry their last tag so an open panel shows a
// scan without a reload.
type uaStatusItem struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Status  string `json:"status"`
	Text    string `json:"text"`
	Pos     string `json:"pos,omitempty"`
	Tag     string `json:"tag,omitempty"`
	TagSeen string `json:"tagSeen,omitempty"`
}

// handleAdminUAStatus serves a lightweight live snapshot of every
// row's status plus the fleet counters as JSON. The Device Center
// polls it so an online/offline (or lock-state) change shows up
// without a manual reload. It uses the UDM's cached device list (no
// refresh=true): this runs every few seconds and must stay cheap.
//
// Each source is isolated: a failing fetch drops its items for this
// poll (the client keeps their last state) and marks the snapshot
// incomplete, which suppresses the counters - partial numbers would
// make the flip displays lie.
func (s *Server) handleAdminUAStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	uaOK := s.uaReady(r.Context())
	protectOK := s.protectReady(r.Context())
	shellyOK := s.shellyReady(r.Context())
	mideaOK := s.mideaReady()
	// Shelly devices are polled directly (one cheap RPC each); start
	// the fan-out now so it overlaps the UniFi fetches below. A device
	// that does not answer IS the offline signal - per-device failure
	// never marks the snapshot incomplete, unlike a failing UniFi
	// list fetch. Buffered channel: the goroutine can never leak.
	shellyCh := make(chan []shellyProbe, 1)
	if shellyOK {
		go func() { shellyCh <- s.probeShelly(r.Context()) }()
	} else {
		shellyCh <- nil
	}
	// The local reader registry is a source of its own: readers keep
	// their live status even with every UniFi integration off. The
	// list is a local SQLite read - cheap enough for the poll. A read
	// error is tracked separately from "no readers": it must mark the
	// snapshot incomplete below, not just drop the source flag.
	var localReaders []readerstore.Reader
	var localErr error
	if s.readerStore != nil {
		if localReaders, localErr = s.readerStore.List(r.Context()); localErr != nil {
			s.log.Warn("device center: status poll readers failed", "err", localErr)
		}
	}
	localOK := len(localReaders) > 0
	if !uaOK && !protectOK && !localOK && !shellyOK && !mideaOK {
		writeUADetailError(w, "No device source is available - UniFi Access, UniFi Protect, Shelly and Midea are off and no local reader is registered.")
		return
	}
	var items []uaStatusItem
	online, offline, total := 0, 0, 0
	complete := true
	addOnline := func(kind, id string, isOnline bool) {
		st, txt := "offline", "Offline"
		if isOnline {
			st, txt = "online", "Online"
			online++
		} else {
			offline++
		}
		total++
		items = append(items, uaStatusItem{Kind: kind, ID: id, Status: st, Text: txt})
	}

	if uaOK {
		if devices, err := s.ua.ListDevices(r.Context()); err != nil {
			s.log.Warn("device center: status poll devices failed", "err", err)
			complete = false
		} else {
			for _, d := range devices {
				addOnline("device", d.ID, d.IsOnline)
			}
			if doors, derr := s.ua.ListDoors(r.Context()); derr != nil {
				s.log.Warn("device center: status poll doors failed", "err", derr)
				complete = false
			} else {
				for _, dr := range doors {
					var row uaRow
					row.LockFromDoor(dr)
					items = append(items, uaStatusItem{
						Kind: "door", ID: dr.ID, Status: row.StatusState, Text: row.StatusText,
						Pos: uaPositionLabel(dr.PositionState(), dr.PositionRaw()),
					})
					total++
				}
			}
		}
	}
	if protectOK {
		if cams, err := s.protect.ListCameras(r.Context()); err != nil {
			s.log.Warn("device center: status poll cameras failed", "err", err)
			complete = false
		} else {
			for _, c := range cams {
				addOnline("camera", c.ID, c.IsOnline())
			}
		}
		if sens, err := s.protect.ListSensors(r.Context()); err != nil {
			s.log.Warn("device center: status poll sensors failed", "err", err)
			complete = false
		} else {
			for _, sn := range sens {
				addOnline("sensor", sn.ID, sn.IsOnline())
			}
		}
	}
	if localErr != nil {
		complete = false
	}
	for _, rd := range localReaders {
		addOnline("rpi-reader", rd.ID, rd.Online)
		if rd.LastUID != "" {
			it := &items[len(items)-1]
			it.Tag = rd.LastUID
			it.TagSeen = readerLastSeenLabel(rd)
		}
	}
	for _, p := range <-shellyCh {
		addOnline("shelly", p.client.Address(), p.err == nil)
	}
	// Midea Climate Controllers: online status comes from the monitor's cached
	// snapshot (no live probe in the poll - the monitor polls on its own tick).
	if mideaOK {
		snap := map[string]bool{}
		if s.mideaMon != nil {
			for _, rr := range s.mideaMon.Snapshot() {
				snap[rr.ID] = rr.Online
			}
		}
		if act, err := s.mideastore.ListActive(r.Context()); err == nil {
			for _, d := range act {
				addOnline("midea", d.ID, snap[d.ID])
			}
		} else {
			complete = false
		}
	}
	// Pending + ignored Shelly rows are shown in the table but never polled
	// (no online/offline contribution). Fold their count into the grand total
	// so the header "shown / total" matches the rendered rows instead of
	// flickering down on the first poll. Only when the snapshot is complete -
	// counts are suppressed otherwise anyway.
	if complete {
		pend, ign := s.shellyLifecycleRows(r.Context())
		total += len(pend) + len(ign)
		if mideaOK {
			if mp, err := s.mideastore.ListPending(r.Context()); err == nil {
				total += len(mp)
			}
			if mi, err := s.mideastore.ListIgnored(r.Context()); err == nil {
				total += len(mi)
			}
		}
	}

	// sources tells the client which integrations this snapshot covers,
	// so it can suppress the counters when the page still shows rows
	// from a source that has since been disabled (stale rows + fresh
	// counts would contradict each other).
	out := map[string]any{
		"ok":      true,
		"items":   items,
		"sources": map[string]bool{"ua": uaOK, "protect": protectOK, "rpi": localOK, "shelly": shellyOK, "midea": mideaOK},
	}
	if complete {
		out["counts"] = map[string]any{"online": online, "offline": offline, "updates": 0, "total": total}
	}
	_ = json.NewEncoder(w).Encode(out)
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

// handleAdminUAProtectCamera lazily serves one camera's full record
// (flattened) as JSON when its panel opens. Served from a fresh list
// fetch - the Integration API's per-id endpoints stay untouched, the
// page needs nothing beyond what the list already carries.
func (s *Server) handleAdminUAProtectCamera(w http.ResponseWriter, r *http.Request) {
	id, ok := s.protectDetailPrelude(w, r)
	if !ok {
		return
	}
	sec := uaSection{Title: "Camera details"}
	if cams, err := s.protect.ListCameras(r.Context()); err != nil {
		s.log.Warn("device center: camera detail failed", "err", err)
		sec.Error = protectFriendlyError(err)
	} else {
		found := false
		for _, c := range cams {
			if c.ID == id {
				sec.Rows = flattenUADetail(anyMap(c.Raw))
				found = true
				break
			}
		}
		if !found {
			sec.Error = "Not found."
		}
	}
	writeUADetail(w, sec)
}

// handleAdminUAProtectSensor lazily serves one sensor's full record
// (flattened) as JSON when its panel opens.
func (s *Server) handleAdminUAProtectSensor(w http.ResponseWriter, r *http.Request) {
	id, ok := s.protectDetailPrelude(w, r)
	if !ok {
		return
	}
	sec := uaSection{Title: "Sensor details"}
	if sens, err := s.protect.ListSensors(r.Context()); err != nil {
		s.log.Warn("device center: sensor detail failed", "err", err)
		sec.Error = protectFriendlyError(err)
	} else {
		found := false
		for _, sn := range sens {
			if sn.ID == id {
				sec.Rows = flattenUADetail(anyMap(sn.Raw))
				found = true
				break
			}
		}
		if !found {
			sec.Error = "Not found."
		}
	}
	writeUADetail(w, sec)
}

// anyMap widens a typed nil map to a JSON-flattenable any (a nil map
// flattens to no rows instead of a "-" scalar).
func anyMap(m map[string]any) any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// protectDetailPrelude mirrors uaDetailPrelude for the Protect lazy
// detail endpoints (JSON headers, protect-ready gate, id validation).
func (s *Server) protectDetailPrelude(w http.ResponseWriter, r *http.Request) (string, bool) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if !s.protectReady(r.Context()) {
		writeUADetailError(w, "UniFi Protect is not active or not configured.")
		return "", false
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if !uaValidID(id) {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return "", false
	}
	return id, true
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
