// Saison 13-05: admin /a/intercom-mapping page.
//
// Operator picks one UA-Access door per UA-Access intercom and
// the result lands in platform_config.intercom_to_door (JSON map
// keyed by intercom-mac in colon-form lowercase). The mieter side
// (POST /einloggen/doors/{intercom_mac}/unlock) already resolves
// via platformconfig.LookupDoorForIntercom; this page is the
// missing UI counterpart so the operator no longer has to write
// the mapping with sqlite.
package httpserver

import (
	"encoding/json"
	"net/http"
	"strings"
)

// adminIntercomMappingData is the page payload for
// templates/admin/intercom-mapping.html. Intercoms and Doors are
// nil when the UA-API call failed; the template shows a helpful
// banner in that case. CurrentMapping is keyed by intercom-mac in
// lowercase colon form, matching what the dropdown options write
// via data-dropdown-value.
//
// Saison 13-06: a second table "Viewer-Standby-Tuer" reads the
// viewer rows from the local mockmanager (no UA-API call) and
// the per-viewer default door from platformconfig.viewer_to_door.
// The two tables are independent: an intercom can ring different
// viewers, and each viewer can pick its own standby door
// regardless of the intercom mapping.
type adminIntercomMappingData struct {
	User           adminUser
	Intercoms      []intercomRow
	Doors          []doorRow
	CurrentMapping map[string]string
	Viewers        []viewerMappingRow
	ViewerMapping  map[string]string
	APIConfigured  bool
	APIError       string
	Flash          string
	FlashType      string
}

type intercomRow struct {
	ID         string
	Name       string
	MAC        string // lowercase colon form
	DeviceType string
	Online     bool
}

type doorRow struct {
	ID    string
	Name  string
	HubID string
}

// viewerMappingRow is the second-table source. Pulled from
// mockmanager.ListViewers (local, no UA call). Type is "web"
// or "esp" and matches the type-column on the admin
// /a/web-viewers and /a/esp-viewers pages.
type viewerMappingRow struct {
	MAC  string
	Name string
	Type string
}

func (s *Server) handleAdminIntercomMappingGet(w http.ResponseWriter, r *http.Request) {
	username := AdminUserFromContext(r.Context())
	data := adminIntercomMappingData{
		User: adminUser{Name: username, Initials: initialsOf(username)},
	}

	currentMapping, err := s.platformCfg.IntercomToDoor(r.Context())
	if err != nil {
		s.log.Warn("intercom-mapping: read current", "err", err)
	}
	if currentMapping == nil {
		currentMapping = map[string]string{}
	}
	data.CurrentMapping = currentMapping

	// Saison 13-06: Viewer-Standby-Mapping. Kommt aus
	// platformconfig (gespeichert) plus mockmanager (Liste aller
	// adoptierten Viewer). Beide Quellen sind lokal, kein UA-Call -
	// die Tabelle rendert auch wenn UA offline ist.
	viewerMapping, err := s.platformCfg.ViewerToDoor(r.Context())
	if err != nil {
		s.log.Warn("intercom-mapping: read viewer mapping", "err", err)
	}
	if viewerMapping == nil {
		viewerMapping = map[string]string{}
	}
	data.ViewerMapping = viewerMapping
	if s.mockMgr != nil {
		viewers, err := s.mockMgr.ListViewers(r.Context())
		if err != nil {
			s.log.Warn("intercom-mapping: list viewers", "err", err)
		}
		for _, v := range viewers {
			data.Viewers = append(data.Viewers, viewerMappingRow{
				MAC:  v.MAC,
				Name: v.Name,
				Type: v.Type,
			})
		}
	}

	if s.ua == nil {
		data.APIError = "UA-API nicht konfiguriert. Bitte zuerst unter Einstellungen Base-URL und Token eintragen."
		s.renderAdminPage(w, "intercom-mapping", data)
		return
	}
	data.APIConfigured = true

	intercoms, err := s.ua.ListIntercoms(r.Context())
	if err != nil {
		s.log.Warn("intercom-mapping: list intercoms", "err", err)
		data.APIError = "UA-API antwortet nicht: " + err.Error()
		s.renderAdminPage(w, "intercom-mapping", data)
		return
	}
	// Saison 13-05-HOTFIX2: surface what came back so the next
	// live-test can see immediately whether the filter is too
	// narrow (intercoms list shorter than expected) or whether
	// the API returned nothing at all.
	s.log.Info("intercom-mapping: list intercoms ok", "count", len(intercoms))
	for i, d := range intercoms {
		if i < 5 {
			s.log.Info("intercom-mapping: intercom row",
				"index", i,
				"id", d.ID,
				"alias", d.Alias,
				"type", d.Type,
				"mac", d.DisplayMAC(),
			)
		}
		data.Intercoms = append(data.Intercoms, intercomRow{
			ID:         d.ID,
			Name:       d.DisplayName(),
			MAC:        d.DisplayMAC(),
			DeviceType: d.Type,
			Online:     d.IsOnline,
		})
	}

	doors, err := s.ua.ListDoors(r.Context())
	if err != nil {
		s.log.Warn("intercom-mapping: list doors", "err", err)
		data.APIError = "UA-API antwortet nicht: " + err.Error()
		s.renderAdminPage(w, "intercom-mapping", data)
		return
	}
	for _, d := range doors {
		data.Doors = append(data.Doors, doorRow{
			ID:    d.ID,
			Name:  d.DisplayName(),
			HubID: d.HubID,
		})
	}

	s.renderAdminPage(w, "intercom-mapping", data)
}

// handleAdminIntercomMappingPost stores the mapping. Body shape:
// {"mapping": {"<intercom-mac>": "<door-uuid>", ...}}. Empty
// values clear the entry; an empty map clears the whole mapping.
func (s *Server) handleAdminIntercomMappingPost(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mapping map[string]string `json:"mapping"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	// SetIntercomToDoor already trims + lowercases keys and drops
	// empty values; pass-through is fine. Defensive copy below
	// just guards against nil so the saved JSON is "{}" instead
	// of "null" which would round-trip differently.
	mapping := body.Mapping
	if mapping == nil {
		mapping = map[string]string{}
	}
	if err := s.platformCfg.SetIntercomToDoor(r.Context(), mapping); err != nil {
		s.log.Error("intercom-mapping save", "err", err)
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	// Logging without the full map - just the count + a single
	// representative key so the audit trail isn't a mac dump.
	first := ""
	for k := range mapping {
		first = k
		break
	}
	s.log.Info("intercom mapping saved",
		"count", len(mapping),
		"first_intercom", strings.ToLower(first),
	)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "count": len(mapping)})
}
