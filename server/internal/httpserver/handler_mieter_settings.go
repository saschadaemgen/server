// Saison 14-01b: tenant-facing settings page.
// Saison 14-02 renamed the URL tree from /einloggen/* to
// /webviewer/*.
// Saison 14-03 extends POST with the auto-screensaver field
// and switches the success path to JSON when the request asks
// for it (the inline-settings mode in the home page consumes
// JSON; the stand-alone /webviewer/settings page keeps the
// 303 redirect).
// Saison 14-03-FIX03 Sub-1a: canonical seconds-field name is
// `auto_screensaver_seconds`; the previous `auto_screensaver`
// remains accepted as a legacy alias.
//
// Routes:
//
//	GET  /webviewer/settings    HTML form with idle-view-mode +
//	                            auto-screensaver + info + logout
//	POST /webviewer/settings    persist idle_view_mode +
//	                            auto_screensaver_seconds; on
//	                            Accept: application/json returns
//	                            {"ok":true,"idle_view_mode":...,
//	                             "auto_screensaver_seconds":...},
//	                            otherwise 303 to /webviewer/
//
// Auth: requireSession (cookie-based). The viewer MAC comes from
// the context value the middleware sets.
package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"carvilon.local/server/internal/mockmanager"
)

// mieterSettingsData is the payload for templates/viewer/settings.html.
type mieterSettingsData struct {
	UnitName               string
	IdleViewMode           string // "screensaver" or "livestream"
	AutoScreensaverSeconds int    // 0 = off, otherwise one of AutoScreensaverSecondsAllowed
	Flash                  string
	FlashType              string
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
	case "",
		mockmanager.IdleViewModeScreensaver,
		mockmanager.IdleViewModeLivestream,
		mockmanager.IdleViewModeScreenOff:
		// Saison 14-XX: 'screen_off' wird vom Web-Viewer akzeptiert,
		// aber im UI nicht als Auswahl angeboten - die Browser-Runtime
		// rendert ihn identisch zu 'screensaver'. Akzeptiert wird er
		// trotzdem damit eine ESP-Cross-Device-Aenderung (Mieter setzt
		// am ESP screen_off, Web-Browser zieht via config.changed
		// nach) keine 400 produziert.
	default:
		http.Error(w,
			"idle_view_mode muss 'screensaver', 'livestream' oder 'screen_off' sein",
			http.StatusBadRequest)
		return
	}

	// Saison 14-03 + 14-03-FIX03 Sub-1a: auto-screensaver.
	// Canonical form field is `auto_screensaver_seconds` (matches
	// the DB column and the JSON response key). The shorter
	// `auto_screensaver` alias from the FIX02 implementation
	// stays accepted as a legacy fallback so any in-flight
	// inline-settings JS still sending the old name keeps working
	// through a browser-cache cycle.
	// Empty / missing field keeps the previous value untouched;
	// a present field always overwrites (and 0 disables the timer).
	var autoSecondsApplied *int
	autoRaw := pickAutoScreensaverField(r.PostForm)
	if autoRaw != "" {
		val, perr := strconv.Atoi(strings.TrimSpace(autoRaw))
		if perr != nil {
			http.Error(w, "auto_screensaver_seconds muss eine ganze Zahl sein", http.StatusBadRequest)
			return
		}
		allowed := false
		for _, v := range mockmanager.AutoScreensaverSecondsAllowed {
			if v == val {
				allowed = true
				break
			}
		}
		if !allowed {
			http.Error(w,
				fmt.Sprintf("auto_screensaver_seconds muss einer von %v sein",
					mockmanager.AutoScreensaverSecondsAllowed),
				http.StatusBadRequest)
			return
		}
		autoSecondsApplied = &val
	}

	if err := s.mockMgr.SetIdleViewMode(r.Context(), mac, mode); err != nil {
		if errors.Is(err, mockmanager.ErrViewerNotFound) {
			http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
			return
		}
		s.log.Error("mieter settings save idle", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "Speichern fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	if autoSecondsApplied != nil {
		if err := s.mockMgr.SetAutoScreensaverSeconds(r.Context(), mac, *autoSecondsApplied); err != nil {
			if errors.Is(err, mockmanager.ErrViewerNotFound) {
				http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
				return
			}
			s.log.Error("mieter settings save auto", "err", err, "mac_prefix", safePrefix(mac))
			http.Error(w, "Speichern fehlgeschlagen.", http.StatusInternalServerError)
			return
		}
	}

	// Saison 14-XX: config.changed broadcasten damit andere Tabs /
	// Browser-Sessions auf demselben viewer_mac und (sobald gepaart)
	// ESP-Geraete ihre Config neu holen. Filter ist pro viewer_mac,
	// kein Cross-Tenant-Leak.
	if s.hub != nil {
		s.hub.BroadcastConfigChanged(r.Context(), mac)
	}

	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		out := map[string]any{
			"ok":             true,
			"idle_view_mode": mode,
		}
		if autoSecondsApplied != nil {
			out["auto_screensaver_seconds"] = *autoSecondsApplied
		}
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	http.Redirect(w, r, "/webviewer/", http.StatusSeeOther)
}

// pickAutoScreensaverField returns the first non-empty value
// from the canonical `auto_screensaver_seconds` form key or the
// FIX02-era `auto_screensaver` alias. Returns "" if neither is
// present, signaling "keep the previously-persisted value".
//
// Saison 14-03-FIX03 Sub-1a.
func pickAutoScreensaverField(form map[string][]string) string {
	if raw, present := form["auto_screensaver_seconds"]; present && len(raw) > 0 && raw[0] != "" {
		return raw[0]
	}
	if raw, present := form["auto_screensaver"]; present && len(raw) > 0 && raw[0] != "" {
		return raw[0]
	}
	return ""
}

func (s *Server) buildMieterSettingsData(r *http.Request) (mieterSettingsData, error) {
	mac := ViewerMACFromContext(r.Context())
	info, err := s.mockMgr.GetViewerInfo(r.Context(), mac)
	if err != nil {
		return mieterSettingsData{}, err
	}
	return mieterSettingsData{
		UnitName:               info.Name,
		IdleViewMode:           info.ResolveIdleViewMode(),
		AutoScreensaverSeconds: info.ResolveAutoScreensaverSeconds(),
	}, nil
}
