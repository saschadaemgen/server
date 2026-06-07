// Admin detail page plus admin-side history endpoints.
//
// Routes:
//
//	GET    /a/viewers/{mac}                  detail page (HTML)
//	GET    /a/viewers/{mac}/history          paged JSON
//	DELETE /a/viewers/{mac}/history/{event}  hard-delete one
//	DELETE /a/viewers/{mac}/history          hard-delete all
//
// The detail page is the unified drill-down view for web and
// ESP viewers (the type is branched in the template). The
// history JSON endpoints return AdminListResult with hidden-
// markers; hard-delete cascades via FK on viewer_hidden_events.
package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"carvilon.local/server/internal/doorhistory"
	"carvilon.local/server/internal/viewermanager"
)

// adminViewerDetailData is the payload for
// templates/admin/viewer-detail.html. Server-side we only render
// the base data + filter mask + skeleton of the history section;
// the history table is fetched by the browser via
// /a/viewers/{mac}/history.json.
type adminViewerDetailData struct {
	User                  adminUser
	MAC                   string
	Name                  string
	Type                  string // "web" | "esp"
	Running               bool
	HasPassword           bool
	HasDeviceToken           bool
	PairedIntercomMAC     string
	StreamProfile         string
	LinkedUAUserID        string
	IdleViewMode          string
	AutoScreensaverSeconds int
	HistoryCaptureEnabled bool
	ESPModel              string
	ESPFwVersion          string
	// ESP-specific settings for the settings auto-save form.
	// Only meaningful when Type == "esp".
	ScreenOffAfterSec int
	BrightnessIdle    int
	Language          string
	ClockLayout       string
	PathMode          string // WEG-Schalter (Saison 19-39): auto|local|cloud
	ResolutionMode    string // Auflösungs-Wahl (Saison 19-42): high|medium|low
	// SettingVisibility is a COMPLETE map (every tenantVisibleSettingKeys
	// entry -> effective visible, default true) for the "dem Mieter
	// anzeigen" toggles. (Saison 19-39)
	SettingVisibility map[string]bool
	BackHref          string // "/a/web-viewers" or "/a/esp-viewers"
	BackLabel         string
	// AssignedDoors is the viewer's 1:n door assignment (Saison
	// 19-30). Rendered server-side as door_id/label; the JS upgrades
	// the labels to live names via /a/doors.json.
	AssignedDoors []viewermanager.DoorAssignment
}

func (s *Server) handleAdminViewerDetail(w http.ResponseWriter, r *http.Request) {
	mac, ok := parseMACPathValue(w, r)
	if !ok {
		return
	}
	info, err := s.viewerMgr.GetViewerInfo(r.Context(), mac)
	if err != nil {
		if errors.Is(err, viewermanager.ErrViewerNotFound) {
			http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
			return
		}
		s.log.Error("admin viewer detail get", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	username := AdminUserFromContext(r.Context())
	data := adminViewerDetailData{
		User:                   adminUser{Name: username, Initials: initialsOf(username)},
		MAC:                    info.MAC,
		Name:                   info.Name,
		Type:                   info.Type,
		Running:                info.Running,
		HasPassword:            info.HasPassword,
		HasDeviceToken:            info.HasDeviceToken,
		PairedIntercomMAC:      info.PairedIntercomMAC,
		StreamProfile:          info.StreamProfile,
		LinkedUAUserID:         info.LinkedUAUserID,
		IdleViewMode:           info.ResolveIdleViewMode(),
		AutoScreensaverSeconds: info.ResolveAutoScreensaverSeconds(),
		HistoryCaptureEnabled:  info.ResolveHistoryCaptureEnabled(),
		ESPModel:               info.ESPModel,
		ESPFwVersion:           info.ESPFwVersion,
		ScreenOffAfterSec:      info.ResolveScreenOffAfterSec(),
		BrightnessIdle:         info.ResolveBrightnessIdle(),
		Language:               info.ResolveLanguage(),
		ClockLayout:            info.ResolveClockLayout(),
		PathMode:               info.ResolvePathMode(),
		ResolutionMode:         info.ResolveResolutionMode(),
	}
	switch info.Type {
	case viewermanager.TypeESP:
		data.BackHref = "/a/esp-viewers"
		data.BackLabel = "ESP-Viewer"
	case viewermanager.TypeAndroid:
		data.BackHref = "/a/android-viewers"
		data.BackLabel = "Android-Viewer"
	default:
		data.BackHref = "/a/web-viewers"
		data.BackLabel = "Web-Viewer"
	}
	// 1:n door assignment for the "Tuer-Zuordnung" section. Best
	// effort: an error degrades to an empty list (the section still
	// renders + lets the admin assign doors). Names are resolved
	// client-side via /a/doors.json.
	if assigned, derr := s.viewerMgr.ListViewerDoors(r.Context(), mac); derr == nil {
		data.AssignedDoors = assigned
	} else {
		s.log.Warn("viewer detail list doors", "err", derr, "mac_prefix", safePrefix(mac))
	}
	// Saison 19-39: per-setting tenant visibility for the toggles. A
	// COMPLETE map (every toggleable key) so the template can index each
	// one; default true (= visible), explicit rows overlaid.
	data.SettingVisibility = make(map[string]bool, len(tenantVisibleSettingKeys))
	for _, k := range tenantVisibleSettingKeys {
		data.SettingVisibility[k] = true
	}
	if explicit, verr := s.viewerMgr.ListViewerSettingVisibility(r.Context(), mac); verr == nil {
		for k, v := range explicit {
			data.SettingVisibility[k] = v
		}
	} else {
		s.log.Warn("viewer detail setting visibility", "err", verr, "mac_prefix", safePrefix(mac))
	}
	s.renderAdminPage(w, "viewer-detail", data)
}

// adminViewerHistoryItem is the row shape returned by
// /a/viewers/{mac}/history. Mirrors mieterHistoryItem plus the
// admin-only hidden_by_viewer + hidden_at fields.
type adminViewerHistoryItem struct {
	ID             int64  `json:"id"`
	CreatedAt      int64  `json:"created_at"`
	When           string `json:"when"`
	DoorName       string `json:"door_name,omitempty"`
	IntercomMAC    string `json:"intercom_mac,omitempty"`
	EventType      string `json:"event_type"`
	HiddenByViewer bool   `json:"hidden_by_viewer"`
	HiddenAt       int64  `json:"hidden_at,omitempty"`
}

type adminViewerHistoryResponse struct {
	Events      []adminViewerHistoryItem `json:"events"`
	TotalCount  int                      `json:"total_count"`
	HiddenCount int                      `json:"hidden_count"`
	HasMore     bool                     `json:"has_more"`
	NextOffset  int                      `json:"next_offset"`
}

func (s *Server) handleAdminViewerHistoryJSON(w http.ResponseWriter, r *http.Request) {
	mac, ok := parseMACPathValue(w, r)
	if !ok {
		return
	}
	if s.history == nil {
		writeAdminViewerHistoryJSON(w, adminViewerHistoryResponse{
			Events: []adminViewerHistoryItem{},
		})
		return
	}
	opts, err := parseHistoryListOpts(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Admin default: 50. Client override via ?limit= is
	// respected (validated to 1..50 in the parser).
	if opts.Limit == 0 {
		opts.Limit = adminViewerHistoryDefaultLimit
	}
	res, err := s.history.AdminListAll(r.Context(), mac, opts)
	if err != nil {
		s.log.Error("admin viewer history", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "history list failed", http.StatusInternalServerError)
		return
	}
	meta := s.loadDoorMeta(r.Context())
	items := make([]adminViewerHistoryItem, 0, len(res.Events))
	for _, ev := range res.Events {
		item := adminViewerHistoryItem{
			ID:             ev.ID,
			CreatedAt:      ev.OccurredAt.Unix(),
			When:           formatGermanWhen(ev.OccurredAt),
			IntercomMAC:    ev.IntercomMAC,
			DoorName:       resolveDoorName(meta, ev.IntercomMAC),
			EventType:      ev.EventType,
			HiddenByViewer: ev.HiddenByViewer,
		}
		if ev.HiddenAt != nil {
			item.HiddenAt = ev.HiddenAt.Unix()
		}
		items = append(items, item)
	}
	next := 0
	if res.HasMore {
		next = opts.Offset + len(items)
	}
	writeAdminViewerHistoryJSON(w, adminViewerHistoryResponse{
		Events:      items,
		TotalCount:  res.TotalCount,
		HiddenCount: res.HiddenCount,
		HasMore:     res.HasMore,
		NextOffset:  next,
	})
}

// adminViewerHistoryDefaultLimit is the page size on the admin
// detail page. Per-page default is 50; ListOptsMaxLimit (50) is
// also the upper bound.
const adminViewerHistoryDefaultLimit = 50

func writeAdminViewerHistoryJSON(w http.ResponseWriter, resp adminViewerHistoryResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleAdminViewerHistoryDeleteOne(w http.ResponseWriter, r *http.Request) {
	mac, ok := parseMACPathValue(w, r)
	if !ok {
		return
	}
	if s.history == nil {
		http.Error(w, "history not configured", http.StatusServiceUnavailable)
		return
	}
	idRaw := r.PathValue("event_id")
	id, err := strconv.ParseInt(idRaw, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "event_id muss eine positive Zahl sein", http.StatusBadRequest)
		return
	}
	if err := s.history.AdminDeleteEvent(r.Context(), mac, id); err != nil {
		if errors.Is(err, doorhistory.ErrNotFound) {
			http.Error(w, "Eintrag nicht gefunden.", http.StatusNotFound)
			return
		}
		s.log.Error("admin delete event", "err", err, "mac_prefix", safePrefix(mac), "event_id", id)
		http.Error(w, "Loeschen fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) handleAdminViewerHistoryDeleteAll(w http.ResponseWriter, r *http.Request) {
	mac, ok := parseMACPathValue(w, r)
	if !ok {
		return
	}
	if s.history == nil {
		http.Error(w, "history not configured", http.StatusServiceUnavailable)
		return
	}
	n, err := s.history.AdminDeleteAllForViewer(r.Context(), mac)
	if err != nil {
		s.log.Error("admin delete all", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "Loeschen fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	// Unread-count broadcast: from the mieter side all entries are
	// gone, the badge falls to 0.
	if s.hub != nil {
		s.hub.BroadcastUnreadCount(r.Context(), mac)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":            true,
		"deleted_count": n,
	})
}

// parseMACPathValue extracts and validates the {mac} path
// parameter. Returns false (with 400-response already written)
// when the MAC does not match the colon-lowercase format. Shared
// helper so all four history handlers do not duplicate the same
// boilerplate.
func parseMACPathValue(w http.ResponseWriter, r *http.Request) (string, bool) {
	mac := strings.ToLower(r.PathValue("mac"))
	if !macFormat.MatchString(mac) {
		http.Error(w, "ungueltige MAC", http.StatusBadRequest)
		return "", false
	}
	return mac, true
}
