// Saison 20 admin endpoints + view builders for the per-viewer function list
// (three-level exposure per function) and the Vorlagen-Zuweisung / Abo frame.
//
//	POST /a/viewers/{mac}/exposure   set one function's exposure (ablöst /visibility)
//	POST /a/viewers/{mac}/template   assign (or clear) the viewer's template
//
// Both mutate via featuregate.Store and broadcast config.changed for the one
// viewer (a template change re-resolves live - no copy on attach). The value
// of the two keep_stream functions keeps flowing through the existing
// POST /a/viewers/{mac}/settings handler; the 6 legacy keys are exposure-only
// here (their value bridge follows later - "Web später").
package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"carvilon.local/server/internal/featuregate"
	"carvilon.local/server/internal/viewermanager"
)

// viewerFeatureRow is one row of the function list: the German label, the
// resolved three-level exposure, the licensed flag and - for keep_stream on a
// stream device only - the live bool value plus a flag that a value editor is
// shown.
type viewerFeatureRow struct {
	Key       string
	Label     string
	Exposure  string // tenant_visible | admin_only | hidden
	Licensed  bool
	HasValue  bool // value editor shown (keep_stream on ESP/Android only)
	BoolValue bool // resolved keep_stream column value (only when HasValue)
}

// viewerTemplateOption is one entry of the "Vorlage zuweisen" dropdown.
type viewerTemplateOption struct {
	ID       int64
	Name     string
	Selected bool
}

// viewerAboView is the read-only Abo frame. ViewerLimit / ValidUntil are
// pre-formatted so the template stays logic-free.
type viewerAboView struct {
	PlanName    string
	ViewerCount int
	ViewerLimit string // "n" or "∞"
	ValidUntil  string // "TT.MM.JJJJ" or "unbefristet"
	OverLimit   bool
}

// featureLabelsDE maps catalog keys to the German UI labels (sentence case,
// house style). Keys without an entry fall back to the raw key.
var featureLabelsDE = map[string]string{
	featuregate.KeyKeepStreamInScreensaver: "Stream im Bildschirmschoner halten",
	featuregate.KeyKeepStreamInScreenOff:   "Stream bei Display-aus halten",
	featuregate.KeyIdleViewMode:            "Idle-Ansicht",
	featuregate.KeyAutoScreensaverSeconds:  "Auto-Bildschirmschoner",
	featuregate.KeyClockLayout:             "Uhr-Anzeige",
	featuregate.KeyLanguage:                "Sprache",
	featuregate.KeyHistoryCaptureEnabled:   "Verlauf-Erfassung",
	featuregate.KeyResolutionMode:          "Auflösung",
	featuregate.KeyPathMode:                "Verbindungsweg",
}

func featureLabelDE(key string) string {
	if l, ok := featureLabelsDE[key]; ok {
		return l
	}
	return key
}

// buildViewerFeatureRows resolves every catalog function for the viewer and
// projects it into the row shape the template renders. Best-effort: an
// unresolved gate (feature store unwired / error) degrades to the model
// default tenant_visible + licensed, so the page always renders.
func (s *Server) buildViewerFeatureRows(ctx context.Context, info *viewermanager.ViewerInfo) []viewerFeatureRow {
	gates, err := s.resolveFeatureGates(ctx, info)
	if err != nil {
		s.log.Warn("viewer detail resolve gates", "err", err, "mac_prefix", safePrefix(info.MAC))
	}
	cat := featuregate.DefaultCatalog()
	rows := make([]viewerFeatureRow, 0, len(cat))
	for _, f := range cat {
		row := viewerFeatureRow{
			Key:      f.Key,
			Label:    featureLabelDE(f.Key),
			Exposure: featuregate.ExposureTenantVisible,
			Licensed: true,
		}
		if eff, ok := gates[f.Key]; ok {
			row.Exposure = eff.Exposure
			row.Licensed = eff.Licensed
		}
		// keep_stream value editor shows on every viewer type (Vorlage: every
		// row has its control). The value is the column-aware (admin-set) value
		// regardless of exposure, so the greyed editor under hidden/bookable
		// still shows what was stored.
		if f.Write != nil {
			row.HasValue = true
			row.BoolValue = keepStreamColumnValue(f.Key, info)
		}
		rows = append(rows, row)
	}
	return rows
}

// buildExposureMaps resolves, per catalog key, the current exposure and the
// licensed flag, so the card grid can render each cell's config-mode trailing
// (active switch + X/check/lock) and lock state. Best-effort: unresolved keys
// default to tenant_visible + licensed.
func (s *Server) buildExposureMaps(ctx context.Context, info *viewermanager.ViewerInfo) (map[string]string, map[string]bool) {
	gates, err := s.resolveFeatureGates(ctx, info)
	if err != nil {
		s.log.Warn("viewer detail exposure maps", "err", err, "mac_prefix", safePrefix(info.MAC))
	}
	exp := make(map[string]string)
	lic := make(map[string]bool)
	for _, f := range featuregate.DefaultCatalog() {
		exp[f.Key] = featuregate.ExposureTenantVisible
		lic[f.Key] = true
		if eff, ok := gates[f.Key]; ok {
			exp[f.Key] = eff.Exposure
			lic[f.Key] = eff.Licensed
		}
	}
	return exp, lic
}

// keepStreamColumnValue returns the column-aware keep_stream value via the
// proven Resolve*() methods (set column, or per-type default when unset).
func keepStreamColumnValue(key string, info *viewermanager.ViewerInfo) bool {
	switch key {
	case featuregate.KeyKeepStreamInScreensaver:
		return info.ResolveKeepStreamInScreensaver()
	case featuregate.KeyKeepStreamInScreenOff:
		return info.ResolveKeepStreamInScreenOff()
	}
	return false
}

// buildTemplateSection returns the dropdown options (current marked Selected),
// whether a template is assigned, and the current template name.
func (s *Server) buildTemplateSection(ctx context.Context, mac string) ([]viewerTemplateOption, bool, string) {
	if s.features == nil {
		return nil, false, ""
	}
	curID, curName, has, err := s.features.ViewerTemplate(ctx, mac)
	if err != nil {
		s.log.Warn("viewer detail viewer template", "err", err, "mac_prefix", safePrefix(mac))
	}
	tmpls, terr := s.features.ListTemplates(ctx)
	if terr != nil {
		s.log.Warn("viewer detail list templates", "err", terr)
		return nil, has, curName
	}
	opts := make([]viewerTemplateOption, 0, len(tmpls))
	for _, t := range tmpls {
		opts = append(opts, viewerTemplateOption{ID: t.ID, Name: t.Name, Selected: has && t.ID == curID})
	}
	return opts, has, curName
}

// buildAboView loads the Abo frame, or nil when no feature store / no license
// row exists (the template then shows "Kein Abo hinterlegt").
func (s *Server) buildAboView(ctx context.Context) *viewerAboView {
	if s.features == nil {
		return nil
	}
	lic, err := s.features.GetLicense(ctx)
	if err != nil {
		s.log.Warn("viewer detail get license", "err", err)
		return nil
	}
	if lic == nil {
		return nil
	}
	count, cerr := s.features.CountViewers(ctx)
	if cerr != nil {
		s.log.Warn("viewer detail count viewers", "err", cerr)
	}
	view := &viewerAboView{
		PlanName:    lic.PlanName,
		ViewerCount: count,
		ViewerLimit: "∞",
		ValidUntil:  "unbefristet",
	}
	if lic.ViewerLimit != nil {
		view.ViewerLimit = strconv.Itoa(*lic.ViewerLimit)
		view.OverLimit = count > *lic.ViewerLimit
	}
	if lic.ValidUntil != nil {
		view.ValidUntil = time.UnixMilli(*lic.ValidUntil).Format("02.01.2006")
	}
	return view
}

// adminViewerExposureRequest is the JSON body for POST
// /a/viewers/{mac}/exposure: one function's three-level exposure.
type adminViewerExposureRequest struct {
	FeatureKey string `json:"feature_key"`
	Exposure   string `json:"exposure"`
}

// handleAdminViewerExposure sets the per-viewer exposure for one catalog
// function (tenant_visible | admin_only | hidden) and broadcasts
// config.changed so every tenant device re-fetches + re-resolves. This is the
// three-level successor to the binary POST /a/viewers/{mac}/visibility.
func (s *Server) handleAdminViewerExposure(w http.ResponseWriter, r *http.Request) {
	mac, ok := parseMACPathValue(w, r)
	if !ok {
		return
	}
	if s.features == nil {
		http.Error(w, "Feature-Store nicht konfiguriert.", http.StatusServiceUnavailable)
		return
	}
	if _, err := s.viewerMgr.GetViewerInfo(r.Context(), mac); err != nil {
		if errors.Is(err, viewermanager.ErrViewerNotFound) {
			http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
			return
		}
		s.log.Error("viewer exposure get viewer", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var body adminViewerExposureRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "ungueltiges JSON", http.StatusBadRequest)
		return
	}
	key := strings.TrimSpace(body.FeatureKey)
	if _, known := featuregate.Lookup(key); !known {
		http.Error(w, "unbekannte Funktion", http.StatusBadRequest)
		return
	}
	exposure := strings.TrimSpace(body.Exposure)
	if !featuregate.ValidExposure(exposure) {
		http.Error(w, "ungueltige Sichtbarkeit", http.StatusBadRequest)
		return
	}
	if err := s.features.SetViewerExposure(r.Context(), mac, key, exposure); err != nil {
		s.log.Error("set viewer exposure", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "Speichern fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	if s.hub != nil {
		s.hub.BroadcastConfigChanged(r.Context(), mac)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":          true,
		"feature_key": key,
		"exposure":    exposure,
	})
}

// adminViewerTemplateRequest is the JSON body for POST
// /a/viewers/{mac}/template. template_id null or 0 clears the assignment.
type adminViewerTemplateRequest struct {
	TemplateID *int64 `json:"template_id"`
}

// handleAdminViewerTemplate assigns (or clears) the viewer's template and
// broadcasts config.changed for this one viewer (its values re-resolve live).
// Editing a template's contents is a separate later page; this only attaches.
func (s *Server) handleAdminViewerTemplate(w http.ResponseWriter, r *http.Request) {
	mac, ok := parseMACPathValue(w, r)
	if !ok {
		return
	}
	if s.features == nil {
		http.Error(w, "Feature-Store nicht konfiguriert.", http.StatusServiceUnavailable)
		return
	}
	if _, err := s.viewerMgr.GetViewerInfo(r.Context(), mac); err != nil {
		if errors.Is(err, viewermanager.ErrViewerNotFound) {
			http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
			return
		}
		s.log.Error("viewer template get viewer", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var body adminViewerTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "ungueltiges JSON", http.StatusBadRequest)
		return
	}
	var assign *int64
	var name string
	if body.TemplateID != nil && *body.TemplateID != 0 {
		// Validate against the live list so a bad id is a clear 400, not an FK 500.
		tmpls, err := s.features.ListTemplates(r.Context())
		if err != nil {
			s.log.Error("viewer template list", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		found := false
		for _, t := range tmpls {
			if t.ID == *body.TemplateID {
				found, name = true, t.Name
				break
			}
		}
		if !found {
			http.Error(w, "Vorlage nicht gefunden.", http.StatusBadRequest)
			return
		}
		assign = body.TemplateID
	}
	if err := s.features.AssignViewerTemplate(r.Context(), mac, assign); err != nil {
		s.log.Error("assign viewer template", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "Speichern fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	if s.hub != nil {
		s.hub.BroadcastConfigChanged(r.Context(), mac)
	}
	var tid int64
	if assign != nil {
		tid = *assign
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":            true,
		"template_id":   tid,
		"template_name": name,
	})
}
