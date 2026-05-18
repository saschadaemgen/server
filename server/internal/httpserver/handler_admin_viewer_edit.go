// Saison 14-04-Phase2-FIX02: Admin-Inline-Edit-Endpoints fuer
// die Detail-Seite. Vier Endpoints, alle JSON-only, alle
// requireAdminSession-gated:
//
//	POST /a/viewers/{mac}/stammdaten
//	POST /a/viewers/{mac}/settings
//	POST /a/viewers/{mac}/password
//	POST /a/viewers/{mac}/regenerate-token
//
// Die ersten beiden triggern doorbellhub.BroadcastConfigChanged
// damit Mieter-Browser ihre Config neu holen (Multi-Device-Sync
// wie bei /webviewer/settings).
package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"carvilon.local/server/internal/auth/esptoken"
	"carvilon.local/server/internal/mockmanager"
)

// adminViewerStammdatenRequest ist der JSON-Body fuer
// /a/viewers/{mac}/stammdaten. Alle Felder optional - der
// Handler patcht nur was im Body steht. Strings werden getrimmt.
// Empty-String an einem Feld setzt das Feld auf "leer" (NULL in
// der DB); zum Nicht-Anfassen das Feld ganz weglassen.
type adminViewerStammdatenRequest struct {
	Name              *string `json:"name,omitempty"`
	PairedIntercomMAC *string `json:"paired_intercom_mac,omitempty"`
	StreamProfile     *string `json:"stream_profile,omitempty"`
	LinkedUAUserID    *string `json:"linked_ua_user_id,omitempty"`
}

// handleAdminViewerStammdaten setzt Name + Paired-Intercom +
// Stream-Profil + UA-User-Verknuepfung auf einem Viewer.
// Validierung pro Feld; broadcastet config.changed nur wenn min.
// ein Feld tatsaechlich geaendert wurde.
func (s *Server) handleAdminViewerStammdaten(w http.ResponseWriter, r *http.Request) {
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
		s.log.Error("stammdaten get viewer", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var body adminViewerStammdatenRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "ungueltiges JSON", http.StatusBadRequest)
		return
	}
	changed := false

	if body.Name != nil {
		name := strings.TrimSpace(*body.Name)
		if name == "" || len(name) > 64 {
			http.Error(w, "Mieter-Name fehlt oder zu lang (max 64 Zeichen).", http.StatusBadRequest)
			return
		}
		if mockmanager.NormalizeName(name) != mockmanager.NormalizeName(info.Name) {
			if err := s.mockMgr.Rename(r.Context(), mac, name); err != nil {
				switch {
				case errors.Is(err, mockmanager.ErrNameInUse):
					http.Error(w, "Wohnungs-Name ist bereits vergeben (case-insensitive).", http.StatusConflict)
				case errors.Is(err, mockmanager.ErrViewerNotFound):
					http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
				default:
					s.log.Error("stammdaten rename", "err", err, "mac_prefix", safePrefix(mac))
					http.Error(w, "Umbenennen fehlgeschlagen.", http.StatusInternalServerError)
				}
				return
			}
			changed = true
		}
	}

	if body.PairedIntercomMAC != nil {
		paired := strings.ToLower(strings.TrimSpace(*body.PairedIntercomMAC))
		if paired != "" && !macFormat.MatchString(paired) {
			http.Error(w, "Klingel-MAC muss lowercase xx:xx:xx:xx:xx:xx sein.", http.StatusBadRequest)
			return
		}
		if paired != info.PairedIntercomMAC {
			if err := s.mockMgr.SetPairedIntercomMAC(r.Context(), mac, paired); err != nil {
				s.respondStammdatenErr(w, mac, "paired_intercom_mac", err)
				return
			}
			changed = true
		}
	}

	if body.StreamProfile != nil {
		profile := strings.TrimSpace(*body.StreamProfile)
		if profile != info.StreamProfile {
			if err := s.mockMgr.SetStreamProfile(r.Context(), mac, profile); err != nil {
				s.respondStammdatenErr(w, mac, "stream_profile", err)
				return
			}
			changed = true
		}
	}

	if body.LinkedUAUserID != nil {
		linked := strings.TrimSpace(*body.LinkedUAUserID)
		if linked != info.LinkedUAUserID {
			if err := s.mockMgr.SetLinkedUAUserID(r.Context(), mac, linked); err != nil {
				s.respondStammdatenErr(w, mac, "linked_ua_user_id", err)
				return
			}
			changed = true
		}
	}

	if changed && s.hub != nil {
		s.hub.BroadcastConfigChanged(r.Context(), mac)
	}

	// Frische Info zurueck liefern damit das UI ohne extra GET
	// die neuen Werte sieht.
	fresh, _ := s.mockMgr.GetViewerInfo(r.Context(), mac)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"changed": changed,
		"viewer":  adminViewerJSON(fresh),
	})
}

// adminViewerSettingsRequest ist der JSON-Body fuer
// /a/viewers/{mac}/settings. Strikt identisch zu /esp/settings
// vom Vokabular, plus history_capture. ESP-spezifische Felder
// sind nur bei type='esp' erlaubt - sonst 400.
type adminViewerSettingsRequest struct {
	IdleViewMode           *string `json:"idle_view_mode,omitempty"`
	AutoScreensaverSeconds *int    `json:"auto_screensaver_seconds,omitempty"`
	HistoryCapture         *bool   `json:"history_capture,omitempty"`
	ClockLayout            *string `json:"clock_layout,omitempty"`
	ScreenOffAfterSec      *int    `json:"screen_off_after_sec,omitempty"`
	BrightnessIdle         *int    `json:"brightness_idle,omitempty"`
	Language               *string `json:"language,omitempty"`
}

// handleAdminViewerSettings ist die Auto-Save-Senke fuer alle
// Settings-Felder. Mieter-Settings (idle/auto-screensaver/history-
// capture) gelten fuer beide Viewer-Typen; ESP-Settings (screen-
// off/brightness/language) sind nur bei type='esp' erlaubt.
func (s *Server) handleAdminViewerSettings(w http.ResponseWriter, r *http.Request) {
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
		s.log.Error("admin settings get viewer", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	isESP := info.Type == mockmanager.TypeESP

	var body adminViewerSettingsRequest
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
		// screen_off ist ein ESP-Hardware-Konzept - bei Web-Viewer
		// erlauben wir es zwar als pass-through (S14-XX-Vertrag,
		// Browser rendert wie screensaver), aber der Admin-Edit
		// im Web-Tab sollte das nicht setzen. Trotzdem akzeptieren
		// damit Sync-Pfad (admin <-> ESP) symmetrisch bleibt.
		if err := s.mockMgr.SetIdleViewMode(r.Context(), mac, v); err != nil {
			s.respondSettingsErr(w, mac, "idle_view_mode", err)
			return
		}
		applied["idle_view_mode"] = v
	}

	if body.AutoScreensaverSeconds != nil {
		v := *body.AutoScreensaverSeconds
		if !slices.Contains(mockmanager.AutoScreensaverSecondsAllowed, v) {
			http.Error(w,
				fmt.Sprintf("auto_screensaver_seconds muss einer von %v sein",
					mockmanager.AutoScreensaverSecondsAllowed),
				http.StatusBadRequest)
			return
		}
		if err := s.mockMgr.SetAutoScreensaverSeconds(r.Context(), mac, v); err != nil {
			s.respondSettingsErr(w, mac, "auto_screensaver_seconds", err)
			return
		}
		applied["auto_screensaver_seconds"] = v
	}

	if body.HistoryCapture != nil {
		if err := s.mockMgr.SetHistoryCaptureEnabled(r.Context(), mac, *body.HistoryCapture); err != nil {
			s.respondSettingsErr(w, mac, "history_capture", err)
			return
		}
		applied["history_capture"] = *body.HistoryCapture
	}

	if body.ClockLayout != nil {
		v := *body.ClockLayout
		if !slices.Contains(mockmanager.ClockLayoutAllowed, v) {
			http.Error(w,
				fmt.Sprintf("clock_layout muss einer von %v sein", mockmanager.ClockLayoutAllowed),
				http.StatusBadRequest)
			return
		}
		if err := s.mockMgr.SetClockLayout(r.Context(), mac, v); err != nil {
			s.respondSettingsErr(w, mac, "clock_layout", err)
			return
		}
		applied["clock_layout"] = v
	}

	// ESP-only Felder. type='web' -> 400 mit klarem Hinweis.
	if body.ScreenOffAfterSec != nil {
		if !isESP {
			http.Error(w, "screen_off_after_sec ist nur fuer ESP-Viewer", http.StatusBadRequest)
			return
		}
		v := *body.ScreenOffAfterSec
		if !slices.Contains(mockmanager.ScreenOffAfterSecAllowed, v) {
			http.Error(w,
				fmt.Sprintf("screen_off_after_sec muss einer von %v sein",
					mockmanager.ScreenOffAfterSecAllowed),
				http.StatusBadRequest)
			return
		}
		if err := s.mockMgr.SetScreenOffAfterSec(r.Context(), mac, v); err != nil {
			s.respondSettingsErr(w, mac, "screen_off_after_sec", err)
			return
		}
		applied["screen_off_after_sec"] = v
	}
	if body.BrightnessIdle != nil {
		if !isESP {
			http.Error(w, "brightness_idle ist nur fuer ESP-Viewer", http.StatusBadRequest)
			return
		}
		v := *body.BrightnessIdle
		if v < 0 || v > 100 {
			http.Error(w, "brightness_idle muss zwischen 0 und 100 liegen",
				http.StatusBadRequest)
			return
		}
		if err := s.mockMgr.SetBrightnessIdle(r.Context(), mac, v); err != nil {
			s.respondSettingsErr(w, mac, "brightness_idle", err)
			return
		}
		applied["brightness_idle"] = v
	}
	if body.Language != nil {
		if !isESP {
			http.Error(w, "language ist nur fuer ESP-Viewer", http.StatusBadRequest)
			return
		}
		v := *body.Language
		if v != "" && !slices.Contains(mockmanager.LanguageAllowed, v) {
			http.Error(w,
				fmt.Sprintf("language muss einer von %v sein", mockmanager.LanguageAllowed),
				http.StatusBadRequest)
			return
		}
		if err := s.mockMgr.SetLanguage(r.Context(), mac, v); err != nil {
			s.respondSettingsErr(w, mac, "language", err)
			return
		}
		applied["language"] = v
	}

	if len(applied) > 0 && s.hub != nil {
		s.hub.BroadcastConfigChanged(r.Context(), mac)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"applied": applied,
	})
}

// adminViewerPasswordRequest ist der JSON-Body fuer
// /a/viewers/{mac}/password. Nur Web-Viewer haben Passwoerter;
// ESP-Viewer benutzen Bearer-Token (siehe regenerate-token).
type adminViewerPasswordRequest struct {
	NewPassword string `json:"new_password"`
}

// handleAdminViewerPassword setzt ein neues Passwort auf einem
// Web-Viewer. Mindestens 8 Zeichen; bestehende Sessions werden
// invalidiert. ESP-Viewer (kein Passwort-Konzept) liefern 400.
func (s *Server) handleAdminViewerPassword(w http.ResponseWriter, r *http.Request) {
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
		s.log.Error("password get viewer", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if info.Type != mockmanager.TypeWeb {
		http.Error(w, "Passwort gibt es nur fuer Web-Viewer. ESP nutzt Bearer-Token.", http.StatusBadRequest)
		return
	}
	var body adminViewerPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "ungueltiges JSON", http.StatusBadRequest)
		return
	}
	if len(body.NewPassword) < 8 {
		http.Error(w, "Passwort muss mindestens 8 Zeichen haben.", http.StatusBadRequest)
		return
	}
	resp, err := s.storePasswordForViewer(r.Context(), info.MAC, info.Name, body.NewPassword, r)
	if err != nil {
		s.log.Error("admin password set", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "Passwort-Speicherung fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	if _, err := s.sessions.RevokeAllForViewer(r.Context(), info.MAC); err != nil {
		s.log.Warn("revoke sessions after admin pw set", "err", err)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":  true,
		"mac": resp.MAC,
	})
}

// adminViewerTokenResponse ist die Antwort von
// /a/viewers/{mac}/regenerate-token. Der frische Klartext-Token
// wird EINMALIG in der Response geliefert; eine zweite Abfrage
// liefert den Token nicht erneut. Briefing 9: "Token einmalig
// im Klartext sichtbar, kann NICHT erneut angezeigt werden".
type adminViewerTokenResponse struct {
	OK        bool   `json:"ok"`
	NewToken  string `json:"new_token"`
	MAC       string `json:"mac"`
}

// handleAdminViewerRegenerateToken erzeugt einen frischen Bearer-
// Token fuer einen ESP-Viewer und gibt den Klartext einmalig
// zurueck. Der existierende handleAdminESPViewersRegenerateToken
// liefert nur einen Preview + parkt den Token in
// esp_pending_devices fuer den ESP-Pickup; das Phase2-FIX02-Briefing
// verlangt den vollen Klartext-Return zur direkten Admin-Anzeige.
// Beide Pfade leben friedlich nebeneinander: dieser hier zeigt
// dem Admin den Wert, der bestehende Handoff-Slot bleibt fuer den
// ESP-Status-Poll bestehen.
func (s *Server) handleAdminViewerRegenerateToken(w http.ResponseWriter, r *http.Request) {
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
		s.log.Error("token regen get viewer", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if info.Type != mockmanager.TypeESP {
		http.Error(w, "Token-Regeneration nur fuer ESP-Viewer.", http.StatusBadRequest)
		return
	}
	clearText, hash, err := esptoken.Generate()
	if err != nil {
		s.log.Error("admin token regen generate", "err", err)
		http.Error(w, "Token-Erzeugung fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	if err := s.mockMgr.SetESPTokenHash(r.Context(), mac, hash); err != nil {
		s.log.Error("admin token regen store", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "Token-Speicherung fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	// Auch in pending-Handoff parken damit ein neuer Status-Poll
	// von einem (noch nicht updateten) ESP den Token automatisch
	// uebernimmt. Parallelbetrieb zum klassischen
	// /a/esp-viewers/{mac}/regenerate-token.
	s.parkTokenForHandoff(r.Context(), mac, clearText)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(adminViewerTokenResponse{
		OK:       true,
		NewToken: clearText,
		MAC:      mac,
	})
}

// parkTokenForHandoff stashes the cleartext token in
// esp_pending_devices.adopted_token_cleartext so a /esp/discover/
// status-poll can pick it up. Best-effort - log + continue on
// failure (the admin-side response already has the cleartext).
func (s *Server) parkTokenForHandoff(ctx context.Context, mac, clearText string) {
	if s.platformCfg == nil {
		return
	}
	_, err := s.platformCfg.DB().ExecContext(ctx,
		`INSERT INTO esp_pending_devices
		   (mac, discovered_at, last_poll_at, adopted_token_cleartext)
		 VALUES (?, strftime('%s','now')*1000, strftime('%s','now')*1000, ?)
		 ON CONFLICT(mac) DO UPDATE SET
		   adopted_token_cleartext = excluded.adopted_token_cleartext,
		   last_poll_at = excluded.last_poll_at,
		   rejected_at = NULL`,
		mac, clearText,
	)
	if err != nil {
		s.log.Warn("admin token regen handoff", "err", err, "mac_prefix", safePrefix(mac))
	}
}

// adminViewerJSON shrinks a ViewerInfo down to the fields the
// Stammdaten-UI needs for re-render after a save. We do NOT
// expose password hash / esp token hash here.
func adminViewerJSON(info *mockmanager.ViewerInfo) map[string]any {
	if info == nil {
		return nil
	}
	return map[string]any{
		"mac":                       info.MAC,
		"name":                      info.Name,
		"type":                      info.Type,
		"paired_intercom_mac":       info.PairedIntercomMAC,
		"stream_profile":            info.StreamProfile,
		"linked_ua_user_id":         info.LinkedUAUserID,
		"idle_view_mode":            info.ResolveIdleViewMode(),
		"auto_screensaver_seconds":  info.ResolveAutoScreensaverSeconds(),
		"screen_off_after_sec":      info.ResolveScreenOffAfterSec(),
		"brightness_idle":           info.ResolveBrightnessIdle(),
		"language":                  info.ResolveLanguage(),
		"history_capture_enabled":   info.ResolveHistoryCaptureEnabled(),
		"clock_layout":              info.ResolveClockLayout(),
		"has_password":              info.HasPassword,
		"has_esp_token":             info.HasESPToken,
	}
}

// respondStammdatenErr wandelt mockmanager-Fehler in deutsche
// 4xx/500-Responses. Pendant zu respondSettingsErr.
func (s *Server) respondStammdatenErr(w http.ResponseWriter, mac, field string, err error) {
	if errors.Is(err, mockmanager.ErrViewerNotFound) {
		http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
		return
	}
	s.log.Error("admin stammdaten save",
		"field", field, "err", err, "mac_prefix", safePrefix(mac))
	http.Error(w, "Speichern fehlgeschlagen.", http.StatusInternalServerError)
}
