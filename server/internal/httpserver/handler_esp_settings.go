// Saison 14-XX: POST /esp/settings.
//
// ESP-Geraet kann seine persistierten Settings aktualisieren -
// idle_view_mode, auto_screensaver_seconds, screen_off_after_sec,
// brightness_idle, language. Alle Felder optional (Partial-
// Update); jedes vorhandene Feld wird strict gegen die Allow-
// Liste in viewermanager geprueft, applied = nur die tatsaechlich
// gesetzten Felder.
//
// Auth: requireESPBearer. MAC kommt aus dem Bearer-Context.
//
// Response: { ok: true, applied: { ...nur geaenderte Felder } }
//
// Triggert doorbellhub.BroadcastConfigChanged damit Mieter-
// Browser (auf demselben viewer_mac via /webviewer/events) und
// andere ESP-Geraete (via /esp/events) ihre Config neu holen.
//
// Saison 14-04-Phase2-FIX06: history_capture (boolean) kommt
// dazu. Toggle den Datenschutz-Schalter fuer die Verlauf-Liste
// (deaktiviert blendet die Mieter-/ESP-API leer, der Server-
// Audit-Trail bleibt intakt; Admin sieht weiter alles). Boolean
// statt Allow-Liste; bei false UND true wird config.changed
// broadcastet.
package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"carvilon.local/server/internal/viewermanager"
)

type espSettingsRequest struct {
	IdleViewMode           *string `json:"idle_view_mode,omitempty"`
	AutoScreensaverSeconds *int    `json:"auto_screensaver_seconds,omitempty"`
	ScreenOffAfterSec      *int    `json:"screen_off_after_sec,omitempty"`
	BrightnessIdle         *int    `json:"brightness_idle,omitempty"`
	Language               *string `json:"language,omitempty"`
	ClockLayout            *string `json:"clock_layout,omitempty"`
	HistoryCapture         *bool   `json:"history_capture,omitempty"`
}

// idleViewModeAllowed mirrors the viewermanager.SetIdleViewMode
// switch so the handler can reject early with a clear German
// error before touching the DB. The list MUST stay in sync with
// the four cases in SetIdleViewMode (empty string, screensaver,
// livestream, screen_off).
var idleViewModeAllowed = []string{
	viewermanager.IdleViewModeScreensaver,
	viewermanager.IdleViewModeLivestream,
	viewermanager.IdleViewModeScreenOff,
}

func (s *Server) handleESPSettings(w http.ResponseWriter, r *http.Request) {
	mac := ESPMACFromContext(r.Context())
	if mac == "" {
		http.Error(w, "no esp identity", http.StatusUnauthorized)
		return
	}
	var body espSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "ungueltiges JSON", http.StatusBadRequest)
		return
	}

	applied := map[string]any{}

	if body.IdleViewMode != nil {
		v := *body.IdleViewMode
		if !slices.Contains(idleViewModeAllowed, v) {
			http.Error(w,
				fmt.Sprintf("idle_view_mode muss einer von %v sein", idleViewModeAllowed),
				http.StatusBadRequest)
			return
		}
		if err := s.viewerMgr.SetIdleViewMode(r.Context(), mac, v); err != nil {
			s.respondSettingsErr(w, mac, "idle_view_mode", err)
			return
		}
		applied["idle_view_mode"] = v
	}

	if body.AutoScreensaverSeconds != nil {
		v := *body.AutoScreensaverSeconds
		if !slices.Contains(viewermanager.AutoScreensaverSecondsAllowed, v) {
			http.Error(w,
				fmt.Sprintf("auto_screensaver_seconds muss einer von %v sein",
					viewermanager.AutoScreensaverSecondsAllowed),
				http.StatusBadRequest)
			return
		}
		if err := s.viewerMgr.SetAutoScreensaverSeconds(r.Context(), mac, v); err != nil {
			s.respondSettingsErr(w, mac, "auto_screensaver_seconds", err)
			return
		}
		applied["auto_screensaver_seconds"] = v
	}

	if body.ScreenOffAfterSec != nil {
		v := *body.ScreenOffAfterSec
		if !slices.Contains(viewermanager.ScreenOffAfterSecAllowed, v) {
			http.Error(w,
				fmt.Sprintf("screen_off_after_sec muss einer von %v sein",
					viewermanager.ScreenOffAfterSecAllowed),
				http.StatusBadRequest)
			return
		}
		if err := s.viewerMgr.SetScreenOffAfterSec(r.Context(), mac, v); err != nil {
			s.respondSettingsErr(w, mac, "screen_off_after_sec", err)
			return
		}
		applied["screen_off_after_sec"] = v
	}

	if body.BrightnessIdle != nil {
		v := *body.BrightnessIdle
		if v < 0 || v > 100 {
			http.Error(w, "brightness_idle muss zwischen 0 und 100 liegen",
				http.StatusBadRequest)
			return
		}
		if err := s.viewerMgr.SetBrightnessIdle(r.Context(), mac, v); err != nil {
			s.respondSettingsErr(w, mac, "brightness_idle", err)
			return
		}
		applied["brightness_idle"] = v
	}

	if body.Language != nil {
		v := *body.Language
		if v != "" && !slices.Contains(viewermanager.LanguageAllowed, v) {
			http.Error(w,
				fmt.Sprintf("language muss einer von %v sein", viewermanager.LanguageAllowed),
				http.StatusBadRequest)
			return
		}
		if err := s.viewerMgr.SetLanguage(r.Context(), mac, v); err != nil {
			s.respondSettingsErr(w, mac, "language", err)
			return
		}
		applied["language"] = v
	}

	if body.ClockLayout != nil {
		v := *body.ClockLayout
		if !slices.Contains(viewermanager.ClockLayoutAllowed, v) {
			http.Error(w,
				fmt.Sprintf("clock_layout muss einer von %v sein", viewermanager.ClockLayoutAllowed),
				http.StatusBadRequest)
			return
		}
		if err := s.viewerMgr.SetClockLayout(r.Context(), mac, v); err != nil {
			s.respondSettingsErr(w, mac, "clock_layout", err)
			return
		}
		applied["clock_layout"] = v
	}

	// Saison 14-04-Phase2-FIX06: Datenschutz-Toggle. Boolean,
	// keine Allow-Liste. SetHistoryCaptureEnabled persistiert,
	// danach loest config.changed im SSE-Block den Refetch in
	// allen Tabs + auf der ESP-Hardware aus.
	if body.HistoryCapture != nil {
		v := *body.HistoryCapture
		if err := s.viewerMgr.SetHistoryCaptureEnabled(r.Context(), mac, v); err != nil {
			s.respondSettingsErr(w, mac, "history_capture", err)
			return
		}
		applied["history_capture"] = v
	}

	// Broadcast config.changed sobald mindestens ein Feld
	// tatsaechlich gesetzt wurde. Leerer POST (kein Feld in body)
	// triggert keinen Broadcast - das waere ein No-Op-Event.
	if len(applied) > 0 && s.hub != nil {
		s.hub.BroadcastConfigChanged(r.Context(), mac)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"applied": applied,
	})
}

// respondSettingsErr maps viewermanager errors to deutsche
// 400/404/500-Responses und logged den vollen Fehler.
func (s *Server) respondSettingsErr(w http.ResponseWriter, mac, field string, err error) {
	if errors.Is(err, viewermanager.ErrViewerNotFound) {
		http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
		return
	}
	s.log.Error("esp settings save",
		"field", field, "err", err, "mac_prefix", safePrefix(mac))
	http.Error(w, "Speichern fehlgeschlagen.", http.StatusInternalServerError)
}
