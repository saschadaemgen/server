// POST /esp/settings.
//
// The ESP device updates its persisted settings - idle_view_mode,
// auto_screensaver_seconds, screen_off_after_sec, brightness_idle,
// language. All fields are optional (partial update); each
// present field is strictly checked against the allow-list in
// viewermanager, applied = only the fields that actually changed.
//
// Auth: requireDeviceBearer. The MAC comes from the bearer context.
//
// Response: { ok: true, applied: { ...only changed fields } }
//
// Triggers doorbellhub.BroadcastConfigChanged so mieter browsers
// (on the same viewer_mac via /webviewer/events) and other ESP
// devices (via /esp/events) refetch their config.
//
// history_capture (boolean) is the privacy toggle for the history
// list (when off, the mieter / ESP API returns an empty list,
// but the server audit trail stays intact; the admin still sees
// everything). It is a boolean instead of an allow-list; both
// false AND true trigger a config.changed broadcast.
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
	// Saison 20: ESP "keep the stream open in the background" flags.
	// Booleans, no allow-list. The JSON keys are part of the ESP
	// contract and must stay verbatim - the firmware's strict
	// allow-list keys exactly on these names.
	KeepStreamInScreensaver *bool `json:"keep_stream_in_screensaver,omitempty"`
	KeepStreamInScreenOff   *bool `json:"keep_stream_in_screen_off,omitempty"`
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
	mac := DeviceMACFromContext(r.Context())
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

	// Privacy toggle. Boolean, no allow-list.
	// SetHistoryCaptureEnabled persists; config.changed in the
	// SSE block then triggers the refetch in all tabs + on the
	// ESP hardware.
	if body.HistoryCapture != nil {
		v := *body.HistoryCapture
		if err := s.viewerMgr.SetHistoryCaptureEnabled(r.Context(), mac, v); err != nil {
			s.respondSettingsErr(w, mac, "history_capture", err)
			return
		}
		applied["history_capture"] = v
	}

	// Keep-stream-in-background flags (Saison 20). Booleans, no
	// allow-list - false AND true are valid; the ESP firmware reads
	// the value and decides whether to hold the stream open.
	if body.KeepStreamInScreensaver != nil {
		v := *body.KeepStreamInScreensaver
		if err := s.viewerMgr.SetKeepStreamInScreensaver(r.Context(), mac, v); err != nil {
			s.respondSettingsErr(w, mac, "keep_stream_in_screensaver", err)
			return
		}
		applied["keep_stream_in_screensaver"] = v
	}

	if body.KeepStreamInScreenOff != nil {
		v := *body.KeepStreamInScreenOff
		if err := s.viewerMgr.SetKeepStreamInScreenOff(r.Context(), mac, v); err != nil {
			s.respondSettingsErr(w, mac, "keep_stream_in_screen_off", err)
			return
		}
		applied["keep_stream_in_screen_off"] = v
	}

	// Broadcast config.changed as soon as at least one field was
	// actually set. An empty POST (no field in the body) does
	// not trigger a broadcast - that would be a no-op event.
	if len(applied) > 0 && s.hub != nil {
		s.hub.BroadcastConfigChanged(r.Context(), mac)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"applied": applied,
	})
}

// respondSettingsErr maps viewermanager errors to German
// 400/404/500 responses and logs the full error.
func (s *Server) respondSettingsErr(w http.ResponseWriter, mac, field string, err error) {
	if errors.Is(err, viewermanager.ErrViewerNotFound) {
		http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
		return
	}
	s.log.Error("esp settings save",
		"field", field, "err", err, "mac_prefix", safePrefix(mac))
	http.Error(w, "Speichern fehlgeschlagen.", http.StatusInternalServerError)
}
