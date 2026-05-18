// Saison 14-04-Phase2: Admin-Detail-Seite + Admin-History-Endpoints.
//
// Routes:
//
//	GET    /a/viewers/{mac}                  Detail-Seite (HTML)
//	GET    /a/viewers/{mac}/history          paged JSON
//	DELETE /a/viewers/{mac}/history/{event}  Hard-Delete einzeln
//	DELETE /a/viewers/{mac}/history          Hard-Delete alle
//
// Die Detail-Seite ist die einheitliche Drill-Down-Sicht fuer
// Web- und ESP-Viewer (Type wird im Template branched). Die
// History-JSON-Endpoints liefern AdminListResult mit
// hidden-Markern; Hard-Delete kaskadiert via FK auf
// viewer_hidden_events.
package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"carvilon.local/server/internal/doorhistory"
	"carvilon.local/server/internal/mockmanager"
)

// adminViewerDetailData ist die Payload fuer
// templates/admin/viewer-detail.html. ServerSide rendern wir nur
// die Stammdaten + Filter-Maske + Skelett der History-Section;
// die History-Tabelle wird via /a/viewers/{mac}/history.json
// vom Browser nachgeladen.
type adminViewerDetailData struct {
	User                  adminUser
	MAC                   string
	Name                  string
	Type                  string // "web" | "esp"
	Running               bool
	HasPassword           bool
	HasESPToken           bool
	PairedIntercomMAC     string
	StreamProfile         string
	LinkedUAUserID        string
	IdleViewMode          string
	AutoScreensaverSeconds int
	HistoryCaptureEnabled bool
	ESPModel              string
	ESPFwVersion          string
	BackHref              string // "/a/web-viewers" oder "/a/esp-viewers"
	BackLabel             string
}

func (s *Server) handleAdminViewerDetail(w http.ResponseWriter, r *http.Request) {
	mac, ok := parseMACPathValue(w, r)
	if !ok {
		return
	}
	info, err := s.mockMgr.GetViewerInfo(r.Context(), mac)
	if err != nil {
		if errors.Is(err, mockmanager.ErrViewerNotFound) {
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
		HasESPToken:            info.HasESPToken,
		PairedIntercomMAC:      info.PairedIntercomMAC,
		StreamProfile:          info.StreamProfile,
		LinkedUAUserID:         info.LinkedUAUserID,
		IdleViewMode:           info.ResolveIdleViewMode(),
		AutoScreensaverSeconds: info.ResolveAutoScreensaverSeconds(),
		HistoryCaptureEnabled:  info.ResolveHistoryCaptureEnabled(),
		ESPModel:               info.ESPModel,
		ESPFwVersion:           info.ESPFwVersion,
	}
	if info.Type == mockmanager.TypeESP {
		data.BackHref = "/a/esp-viewers"
		data.BackLabel = "ESP-Viewer"
	} else {
		data.BackHref = "/a/web-viewers"
		data.BackLabel = "Web-Viewer"
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
	// Admin-Default: 50 (briefing 8). Client-Override per ?limit=
	// bleibt respektiert (validiert auf 1..50 im Parser).
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

// adminViewerHistoryDefaultLimit ist die Page-Size auf der
// Admin-Detail-Seite. Briefing 8 verlangt 50 Pro-Page-Default;
// ListOptsMaxLimit (50) ist gleichzeitig die Obergrenze.
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
	// Unread-Count broadcast: pro Mieter-Side sind alle Eintraege
	// weg, der Badge faellt auf 0.
	if s.hub != nil {
		s.hub.BroadcastUnreadCount(r.Context(), mac)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":            true,
		"deleted_count": n,
	})
}

// parseMACPathValue extrahiert und validiert den {mac}-Pfad-
// Parameter. Liefert false (mit 400-Response geschrieben) wenn die
// MAC nicht dem colon-lowercase-Format entspricht. Gemeinsamer
// Helper damit alle vier History-Handler nicht denselben Boilerplate
// duplizieren.
func parseMACPathValue(w http.ResponseWriter, r *http.Request) (string, bool) {
	mac := strings.ToLower(r.PathValue("mac"))
	if !macFormat.MatchString(mac) {
		http.Error(w, "ungueltige MAC", http.StatusBadRequest)
		return "", false
	}
	return mac, true
}
