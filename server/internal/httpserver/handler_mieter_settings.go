// Tenant-facing settings page. POST persists idle_view_mode +
// auto_screensaver_seconds and switches the success path to JSON
// when the request asks for it (the inline-settings mode in the
// home page consumes JSON; the stand-alone /webviewer/settings
// page keeps the 303 redirect).
//
// The canonical seconds-field name is `auto_screensaver_seconds`;
// the previous `auto_screensaver` remains accepted as a legacy
// alias.
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
// Auth: requireViewerAuth (cookie-based). The viewer MAC comes from
// the context value the middleware sets.
package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"carvilon.local/server/internal/viewermanager"
)

// mieterSettingsData is the payload for templates/viewer/settings.html.
//
// HistoryCaptureEnabled hydrates the "Verlauf-Erfassung" radio
// group in the inline settings mode and the standalone
// /webviewer/settings page. ClockLayout carries the clock-display
// preference (vertical / horizontal).
type mieterSettingsData struct {
	UnitName               string
	IdleViewMode           string // "screensaver" or "livestream"
	AutoScreensaverSeconds int    // 0 = off, otherwise one of AutoScreensaverSecondsAllowed
	HistoryCaptureEnabled  bool   // true = mieter sees the history
	ClockLayout            string // "vertical" or "horizontal"
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

// mieterSettingsJSON is the JSON shape GET /webviewer/settings.json
// returns - the JSON-refetch half of the config.changed contract
// (Saison 19-37). Field names mirror /esp/config's "ui" block; the
// values come from the SAME ViewerInfo.Resolve*() the HTML form and
// /esp/config use (one source of truth, no drift). ESP-hardware
// fields (screen_off_after_sec, brightness_idle, stream, weather) are
// deliberately omitted - a phone does not need them. The schema is
// extensible: path_mode lands here additively with the WEG switch
// (S19-33).
type mieterSettingsJSON struct {
	IdleViewMode           string `json:"idle_view_mode"`
	AutoScreensaverSeconds int    `json:"auto_screensaver_seconds"`
	ClockLayout            string `json:"clock_layout"`
	Language               string `json:"language"`
	HistoryCaptureEnabled  bool   `json:"history_capture_enabled"`
	// PathMode is the transport-path override (WEG-Schalter, S19-39):
	// "auto" | "local" | "cloud". The app honours it when choosing the
	// edge-vs-cloud endpoint; v1 is admin-set.
	PathMode string `json:"path_mode"`
	// ResolutionMode is the source-resolution choice (Saison 19-42):
	// "high" | "medium" | "low". The stream pulls it + the app uses it at
	// stream-start; v1 is admin-set (weg-abhaengige LAN=high later).
	ResolutionMode string `json:"resolution_mode"`
	UnitName       string `json:"unit_name"`
	// Visibility maps setting_key -> whether the tenant may see/change the
	// control (Saison 19-39). EXPLICIT rows only; a missing key = visible
	// (default). omitempty -> the key is absent when there are no
	// overrides, so the flat S19-37 contract stays byte-identical for
	// unconfigured viewers. The flat values above are NEVER omitted - the
	// app still applies a value even when its control is hidden.
	Visibility map[string]bool `json:"visibility,omitempty"`
}

// handleMieterSettingsJSON returns the authenticated viewer's settings
// as JSON. The viewer is identified by requireViewerAuth (Bearer for
// android/esp, cookie for web) -> ViewerMACFromContext; the client
// NEVER sends a MAC. This is what the app refetches on config.changed
// (the SSE/eventbus/FCM legs carry only the signal). Mirrors
// /webviewer/doors. The HTML GET /webviewer/settings is untouched.
func (s *Server) handleMieterSettingsJSON(w http.ResponseWriter, r *http.Request) {
	mac := ViewerMACFromContext(r.Context())
	if mac == "" {
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}
	info, err := s.viewerMgr.GetViewerInfo(r.Context(), mac)
	if err != nil {
		if errors.Is(err, viewermanager.ErrViewerNotFound) {
			http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
			return
		}
		s.log.Error("mieter settings json", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Per-setting visibility (Saison 19-39). Non-fatal: on error fall
	// through with no map -> everything visible (default).
	vis, verr := s.viewerMgr.ListViewerSettingVisibility(r.Context(), mac)
	if verr != nil {
		s.log.Warn("mieter settings json visibility", "err", verr, "mac_prefix", safePrefix(mac))
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(mieterSettingsJSON{
		IdleViewMode:           info.ResolveIdleViewMode(),
		AutoScreensaverSeconds: info.ResolveAutoScreensaverSeconds(),
		ClockLayout:            info.ResolveClockLayout(),
		Language:               info.ResolveLanguage(),
		HistoryCaptureEnabled:  info.ResolveHistoryCaptureEnabled(),
		PathMode:               info.ResolvePathMode(),
		ResolutionMode:         info.ResolveResolutionMode(),
		UnitName:               info.Name,
		Visibility:             vis,
	})
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
		viewermanager.IdleViewModeScreensaver,
		viewermanager.IdleViewModeLivestream,
		viewermanager.IdleViewModeScreenOff:
		// 'screen_off' is accepted by the web viewer but not
		// offered in its UI - the browser runtime renders it
		// identically to 'screensaver'. We still accept it so an
		// ESP-side change (mieter sets screen_off on the ESP,
		// web browser pulls in via config.changed) does not
		// produce a 400.
	default:
		http.Error(w,
			"idle_view_mode muss 'screensaver', 'livestream' oder 'screen_off' sein",
			http.StatusBadRequest)
		return
	}

	// Auto-screensaver. The canonical form field is
	// `auto_screensaver_seconds` (matches the DB column and the
	// JSON response key). The shorter `auto_screensaver` alias
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
		for _, v := range viewermanager.AutoScreensaverSecondsAllowed {
			if v == val {
				allowed = true
				break
			}
		}
		if !allowed {
			http.Error(w,
				fmt.Sprintf("auto_screensaver_seconds muss einer von %v sein",
					viewermanager.AutoScreensaverSecondsAllowed),
				http.StatusBadRequest)
			return
		}
		autoSecondsApplied = &val
	}

	if err := s.viewerMgr.SetIdleViewMode(r.Context(), mac, mode); err != nil {
		if errors.Is(err, viewermanager.ErrViewerNotFound) {
			http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
			return
		}
		s.log.Error("mieter settings save idle", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "Speichern fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	if autoSecondsApplied != nil {
		if err := s.viewerMgr.SetAutoScreensaverSeconds(r.Context(), mac, *autoSecondsApplied); err != nil {
			if errors.Is(err, viewermanager.ErrViewerNotFound) {
				http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
				return
			}
			s.log.Error("mieter settings save auto", "err", err, "mac_prefix", safePrefix(mac))
			http.Error(w, "Speichern fehlgeschlagen.", http.StatusInternalServerError)
			return
		}
	}

	// clock_layout. Accepts the two allow-list values; missing =
	// unchanged.
	var clockLayoutApplied *string
	if raw, present := r.PostForm["clock_layout"]; present && len(raw) > 0 && raw[0] != "" {
		v := strings.TrimSpace(raw[0])
		allowed := false
		for _, opt := range viewermanager.ClockLayoutAllowed {
			if opt == v {
				allowed = true
				break
			}
		}
		if !allowed {
			http.Error(w,
				fmt.Sprintf("clock_layout muss einer von %v sein", viewermanager.ClockLayoutAllowed),
				http.StatusBadRequest)
			return
		}
		clockLayoutApplied = &v
	}

	// history_capture toggle. Accepts "1" and "0" as string form
	// values (the HTML radio input delivers exactly that).
	// Missing = unchanged, invalid = 400.
	var captureApplied *bool
	if raw, present := r.PostForm["history_capture"]; present && len(raw) > 0 && raw[0] != "" {
		switch strings.TrimSpace(raw[0]) {
		case "1", "true":
			t := true
			captureApplied = &t
		case "0", "false":
			f := false
			captureApplied = &f
		default:
			http.Error(w, "history_capture muss 0 oder 1 sein", http.StatusBadRequest)
			return
		}
	}
	if captureApplied != nil {
		if err := s.viewerMgr.SetHistoryCaptureEnabled(r.Context(), mac, *captureApplied); err != nil {
			if errors.Is(err, viewermanager.ErrViewerNotFound) {
				http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
				return
			}
			s.log.Error("mieter settings save capture", "err", err, "mac_prefix", safePrefix(mac))
			http.Error(w, "Speichern fehlgeschlagen.", http.StatusInternalServerError)
			return
		}
	}
	if clockLayoutApplied != nil {
		if err := s.viewerMgr.SetClockLayout(r.Context(), mac, *clockLayoutApplied); err != nil {
			if errors.Is(err, viewermanager.ErrViewerNotFound) {
				http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
				return
			}
			s.log.Error("mieter settings save clock", "err", err, "mac_prefix", safePrefix(mac))
			http.Error(w, "Speichern fehlgeschlagen.", http.StatusInternalServerError)
			return
		}
	}

	// Broadcast config.changed so other tabs / browser sessions
	// on the same viewer_mac and (once paired) ESP devices
	// refetch their config. The filter is per viewer_mac, so no
	// cross-tenant leak.
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
		if captureApplied != nil {
			out["history_capture"] = *captureApplied
		}
		if clockLayoutApplied != nil {
			out["clock_layout"] = *clockLayoutApplied
		}
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	http.Redirect(w, r, "/webviewer/", http.StatusSeeOther)
}

// pickAutoScreensaverField returns the first non-empty value
// from the canonical `auto_screensaver_seconds` form key or the
// legacy `auto_screensaver` alias. Returns "" if neither is
// present, signaling "keep the previously-persisted value".
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
	info, err := s.viewerMgr.GetViewerInfo(r.Context(), mac)
	if err != nil {
		return mieterSettingsData{}, err
	}
	return mieterSettingsData{
		UnitName:               info.Name,
		IdleViewMode:           info.ResolveIdleViewMode(),
		AutoScreensaverSeconds: info.ResolveAutoScreensaverSeconds(),
		HistoryCaptureEnabled:  info.ResolveHistoryCaptureEnabled(),
		ClockLayout:            info.ResolveClockLayout(),
	}, nil
}
