// Saison 14-01b: tenant-facing settings page.
// Saison 14-02 renamed the URL tree from /einloggen/* to
// /webviewer/*; the handler and form are unchanged.
//
// Routes:
//   GET  /webviewer/settings    HTML form with idle-view-mode +
//                               info section + logout link
//   POST /webviewer/settings    persist idle_view_mode, redirect
//                               back to /webviewer/
//
// Auth: requireSession (cookie-based). The viewer MAC comes from
// the context value the middleware sets.
package httpserver

import (
	"errors"
	"net/http"
	"strings"

	"unifix.local/server/internal/mockmanager"
)

// mieterSettingsData is the payload for templates/viewer/settings.html.
type mieterSettingsData struct {
	UnitName     string
	IdleViewMode string // "screensaver" or "livestream"
	Flash        string
	FlashType    string
}

func (s *Server) handleMieterSettingsGet(w http.ResponseWriter, r *http.Request) {
	data, err := s.buildMieterSettingsData(r)
	if err != nil {
		s.log.Error("mieter settings get", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.renderViewer(w, "settings", data); err != nil {
		s.log.Error("render mieter settings", "err", err)
	}
}

func (s *Server) handleMieterSettingsPost(w http.ResponseWriter, r *http.Request) {
	mac := ViewerMACFromContext(r.Context())
	if mac == "" {
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	mode := strings.TrimSpace(r.PostForm.Get("idle_view_mode"))
	switch mode {
	case "", mockmanager.IdleViewModeScreensaver, mockmanager.IdleViewModeLivestream:
	default:
		http.Error(w, "idle_view_mode muss 'screensaver' oder 'livestream' sein", http.StatusBadRequest)
		return
	}
	if err := s.mockMgr.SetIdleViewMode(r.Context(), mac, mode); err != nil {
		if errors.Is(err, mockmanager.ErrViewerNotFound) {
			http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
			return
		}
		s.log.Error("mieter settings save", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "Speichern fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/webviewer/", http.StatusSeeOther)
}

func (s *Server) buildMieterSettingsData(r *http.Request) (mieterSettingsData, error) {
	mac := ViewerMACFromContext(r.Context())
	info, err := s.mockMgr.GetViewerInfo(r.Context(), mac)
	if err != nil {
		return mieterSettingsData{}, err
	}
	return mieterSettingsData{
		UnitName:     info.Name,
		IdleViewMode: info.ResolveIdleViewMode(),
	}, nil
}
