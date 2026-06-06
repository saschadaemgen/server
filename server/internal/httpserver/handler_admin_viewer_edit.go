// Admin inline-edit endpoints for the per-viewer detail page.
// Four endpoints, all JSON-only, all requireAdminSession-gated:
//
//	POST /a/viewers/{mac}/stammdaten
//	POST /a/viewers/{mac}/settings
//	POST /a/viewers/{mac}/password
//	POST /a/viewers/{mac}/regenerate-token
//
// The first two trigger doorbellhub.BroadcastConfigChanged so
// tenant browsers refetch their config (multi-device sync,
// matching /webviewer/settings).
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
	"carvilon.local/server/internal/normalize"
	"carvilon.local/server/internal/viewermanager"
)

// adminViewerStammdatenRequest is the JSON body for
// /a/viewers/{mac}/stammdaten. All fields are optional - the
// handler only patches what is present in the body. Strings are
// trimmed. An empty-string value clears the field (NULL in the
// DB); to leave a field untouched, omit it entirely.
type adminViewerStammdatenRequest struct {
	Name              *string `json:"name,omitempty"`
	PairedIntercomMAC *string `json:"paired_intercom_mac,omitempty"`
	StreamProfile     *string `json:"stream_profile,omitempty"`
	LinkedUAUserID    *string `json:"linked_ua_user_id,omitempty"`
}

// handleAdminViewerStammdaten sets name + paired intercom +
// stream profile + UA-user link on a viewer. Validation is
// per-field; config.changed is broadcast only when at least one
// field actually changed.
func (s *Server) handleAdminViewerStammdaten(w http.ResponseWriter, r *http.Request) {
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
		if normalize.ViewerName(name) != normalize.ViewerName(info.Name) {
			if err := s.viewerMgr.Rename(r.Context(), mac, name); err != nil {
				switch {
				case errors.Is(err, viewermanager.ErrNameInUse):
					http.Error(w, "Wohnungs-Name ist bereits vergeben (case-insensitive).", http.StatusConflict)
				case errors.Is(err, viewermanager.ErrViewerNotFound):
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
			if err := s.viewerMgr.SetPairedIntercomMAC(r.Context(), mac, paired); err != nil {
				s.respondStammdatenErr(w, mac, "paired_intercom_mac", err)
				return
			}
			changed = true
		}
	}

	if body.StreamProfile != nil {
		profile := strings.TrimSpace(*body.StreamProfile)
		if profile != info.StreamProfile {
			if err := s.viewerMgr.SetStreamProfile(r.Context(), mac, profile); err != nil {
				s.respondStammdatenErr(w, mac, "stream_profile", err)
				return
			}
			changed = true
		}
	}

	if body.LinkedUAUserID != nil {
		linked := strings.TrimSpace(*body.LinkedUAUserID)
		if linked != info.LinkedUAUserID {
			if err := s.viewerMgr.SetLinkedUAUserID(r.Context(), mac, linked); err != nil {
				s.respondStammdatenErr(w, mac, "linked_ua_user_id", err)
				return
			}
			changed = true
		}
	}

	if changed && s.hub != nil {
		s.hub.BroadcastConfigChanged(r.Context(), mac)
	}

	// Return the fresh info so the UI sees the new values without
	// an extra GET round-trip.
	fresh, _ := s.viewerMgr.GetViewerInfo(r.Context(), mac)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"changed": changed,
		"viewer":  adminViewerJSON(fresh),
	})
}

// adminDoorAssignment is one door in the per-viewer 1:n assignment,
// in the wire shape both POST /a/viewers/{mac}/doors and the save
// response use. Saison 19-30.
type adminDoorAssignment struct {
	DoorID string `json:"door_id"`
	Label  string `json:"label,omitempty"`
	Sort   int    `json:"sort,omitempty"`
}

// adminViewerDoorsRequest is the JSON body for POST
// /a/viewers/{mac}/doors: the FULL desired door list (replace-all
// semantics, mirroring SetViewerDoors).
type adminViewerDoorsRequest struct {
	Doors []adminDoorAssignment `json:"doors"`
}

// handleAdminViewerDoors replaces a viewer's 1:n door assignment
// (viewer_doors) and broadcasts config.changed so the viewer/app
// refetches its config. Works identically for all three viewer
// types. The door UUIDs are NOT validated against the live UA-API
// here - an admin may assign a door that is briefly unreachable;
// the unlock path resolves+authorises at open-time.
func (s *Server) handleAdminViewerDoors(w http.ResponseWriter, r *http.Request) {
	mac, ok := parseMACPathValue(w, r)
	if !ok {
		return
	}
	if _, err := s.viewerMgr.GetViewerInfo(r.Context(), mac); err != nil {
		if errors.Is(err, viewermanager.ErrViewerNotFound) {
			http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
			return
		}
		s.log.Error("viewer doors get viewer", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var body adminViewerDoorsRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "ungueltiges JSON", http.StatusBadRequest)
		return
	}
	assignments := make([]viewermanager.DoorAssignment, 0, len(body.Doors))
	for i, d := range body.Doors {
		doorID := strings.TrimSpace(d.DoorID)
		if doorID == "" {
			continue
		}
		order := d.Sort
		if order == 0 {
			order = i
		}
		assignments = append(assignments, viewermanager.DoorAssignment{
			DoorID: doorID,
			Label:  strings.TrimSpace(d.Label),
			Sort:   order,
		})
	}
	// Guard (S19-32): the one-step UI uses the per-door Add/Remove
	// endpoints, so the only way to reach this replace-all with an
	// empty list is a direct API call. Refuse it - an empty
	// replace-all silently wiped the assignment (the S19-31 bug).
	// Clearing is done per door via DELETE.
	if len(assignments) == 0 {
		http.Error(w, "leere Tuer-Liste abgelehnt: einzelne Tueren per DELETE entfernen", http.StatusBadRequest)
		return
	}
	if err := s.viewerMgr.SetViewerDoors(r.Context(), mac, assignments); err != nil {
		s.log.Error("set viewer doors", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "Speichern fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	if s.hub != nil {
		s.hub.BroadcastConfigChanged(r.Context(), mac)
	}
	fresh, _ := s.viewerMgr.ListViewerDoors(r.Context(), mac)
	out := make([]adminDoorAssignment, 0, len(fresh))
	for _, d := range fresh {
		out = append(out, adminDoorAssignment{DoorID: d.DoorID, Label: d.Label, Sort: d.Sort})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":    true,
		"doors": out,
	})
}

// handleAdminViewerAddDoor assigns ONE door to a viewer (idempotent)
// - the one-step UI's "add" action (dropdown change persists at
// once). The display name rides as ?label= so the chip shows it
// without a UA round-trip and survives a later UA outage. Broadcasts
// config.changed like the replace-all save. (Saison 19-32)
func (s *Server) handleAdminViewerAddDoor(w http.ResponseWriter, r *http.Request) {
	mac, ok := parseMACPathValue(w, r)
	if !ok {
		return
	}
	doorID := strings.TrimSpace(r.PathValue("door_id"))
	if doorID == "" {
		http.Error(w, "door_id required", http.StatusBadRequest)
		return
	}
	if _, err := s.viewerMgr.GetViewerInfo(r.Context(), mac); err != nil {
		if errors.Is(err, viewermanager.ErrViewerNotFound) {
			http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
			return
		}
		s.log.Error("add viewer door get viewer", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.viewerMgr.AddViewerDoor(r.Context(), mac, viewermanager.DoorAssignment{
		DoorID: doorID,
		Label:  strings.TrimSpace(r.URL.Query().Get("label")),
	}); err != nil {
		s.log.Error("add viewer door", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "Hinzufuegen fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	if s.hub != nil {
		s.hub.BroadcastConfigChanged(r.Context(), mac)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// handleAdminViewerRemoveDoor unassigns ONE door (no error if it was
// not assigned) - the one-step UI's "remove" action. Broadcasts
// config.changed. (Saison 19-32)
func (s *Server) handleAdminViewerRemoveDoor(w http.ResponseWriter, r *http.Request) {
	mac, ok := parseMACPathValue(w, r)
	if !ok {
		return
	}
	doorID := strings.TrimSpace(r.PathValue("door_id"))
	if doorID == "" {
		http.Error(w, "door_id required", http.StatusBadRequest)
		return
	}
	if err := s.viewerMgr.RemoveViewerDoor(r.Context(), mac, doorID); err != nil {
		s.log.Error("remove viewer door", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "Entfernen fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	if s.hub != nil {
		s.hub.BroadcastConfigChanged(r.Context(), mac)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// adminViewerVisibilityRequest is the JSON body for POST
// /a/viewers/{mac}/visibility: one setting's tenant visibility.
type adminViewerVisibilityRequest struct {
	SettingKey string `json:"setting_key"`
	Visible    bool   `json:"visible"`
}

// tenantVisibleSettingKeys are the settings the admin can hide from the
// tenant ("dem Mieter anzeigen"). The DB column is free-text, but the
// admin UI + the detail-page default-map cover exactly these. (S19-39)
var tenantVisibleSettingKeys = []string{
	"idle_view_mode",
	"auto_screensaver_seconds",
	"clock_layout",
	"language",
	"history_capture_enabled",
}

// handleAdminViewerVisibility upserts one per-setting tenant-visibility
// flag (Saison 19-39) and broadcasts config.changed so the app refetches
// and shows/hides the control. Works for all viewer types. The stored
// VALUE is unaffected - this only gates whether the tenant sees/changes
// the control. setting_key is free-text (premium-extensible).
func (s *Server) handleAdminViewerVisibility(w http.ResponseWriter, r *http.Request) {
	mac, ok := parseMACPathValue(w, r)
	if !ok {
		return
	}
	if _, err := s.viewerMgr.GetViewerInfo(r.Context(), mac); err != nil {
		if errors.Is(err, viewermanager.ErrViewerNotFound) {
			http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
			return
		}
		s.log.Error("viewer visibility get viewer", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var body adminViewerVisibilityRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "ungueltiges JSON", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.SettingKey) == "" {
		http.Error(w, "setting_key required", http.StatusBadRequest)
		return
	}
	if err := s.viewerMgr.SetViewerSettingVisibility(r.Context(), mac, body.SettingKey, body.Visible); err != nil {
		s.log.Error("set viewer setting visibility", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "Speichern fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	if s.hub != nil {
		s.hub.BroadcastConfigChanged(r.Context(), mac)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// adminViewerSettingsRequest is the JSON body for
// /a/viewers/{mac}/settings. Vocabulary is strictly identical to
// /esp/settings, plus history_capture. ESP-specific fields are
// only allowed for type='esp' - otherwise 400.
type adminViewerSettingsRequest struct {
	IdleViewMode           *string `json:"idle_view_mode,omitempty"`
	AutoScreensaverSeconds *int    `json:"auto_screensaver_seconds,omitempty"`
	HistoryCapture         *bool   `json:"history_capture,omitempty"`
	ClockLayout            *string `json:"clock_layout,omitempty"`
	ScreenOffAfterSec      *int    `json:"screen_off_after_sec,omitempty"`
	BrightnessIdle         *int    `json:"brightness_idle,omitempty"`
	Language               *string `json:"language,omitempty"`
	PathMode               *string `json:"path_mode,omitempty"`
}

// handleAdminViewerSettings is the auto-save sink for every
// settings field. Tenant-side settings (idle / auto-screensaver
// / history-capture) apply to both viewer types; ESP-side
// settings (screen-off / brightness / language) are only
// accepted for type='esp'.
func (s *Server) handleAdminViewerSettings(w http.ResponseWriter, r *http.Request) {
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
		s.log.Error("admin settings get viewer", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	isESP := info.Type == viewermanager.TypeESP

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
		// screen_off is an ESP-hardware concept. For web viewers
		// we still accept it as a pass-through (the browser
		// renders it identical to screensaver), even though the
		// admin web tab should not normally set it. Accepting
		// the value keeps the admin <-> ESP sync path symmetric.
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

	if body.HistoryCapture != nil {
		if err := s.viewerMgr.SetHistoryCaptureEnabled(r.Context(), mac, *body.HistoryCapture); err != nil {
			s.respondSettingsErr(w, mac, "history_capture", err)
			return
		}
		applied["history_capture"] = *body.HistoryCapture
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

	// Saison 19-39: transport-path override (WEG-Schalter). Applies to
	// all viewer types (not ESP-gated). Validated against the allow-list
	// here so bad input is a 400, not a 500.
	if body.PathMode != nil {
		v := *body.PathMode
		if !slices.Contains(viewermanager.PathModeAllowed, v) {
			http.Error(w,
				fmt.Sprintf("path_mode muss einer von %v sein", viewermanager.PathModeAllowed),
				http.StatusBadRequest)
			return
		}
		if err := s.viewerMgr.SetPathMode(r.Context(), mac, v); err != nil {
			s.respondSettingsErr(w, mac, "path_mode", err)
			return
		}
		applied["path_mode"] = v
	}

	// ESP-only fields. type='web' -> 400 with a clear message.
	if body.ScreenOffAfterSec != nil {
		if !isESP {
			http.Error(w, "screen_off_after_sec ist nur fuer ESP-Viewer", http.StatusBadRequest)
			return
		}
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
		if err := s.viewerMgr.SetBrightnessIdle(r.Context(), mac, v); err != nil {
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

	if len(applied) > 0 && s.hub != nil {
		s.hub.BroadcastConfigChanged(r.Context(), mac)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"applied": applied,
	})
}

// adminViewerPasswordRequest is the JSON body for
// /a/viewers/{mac}/password. Only web viewers carry passwords;
// ESP viewers use bearer tokens (see regenerate-token).
type adminViewerPasswordRequest struct {
	NewPassword string `json:"new_password"`
}

// handleAdminViewerPassword sets a new password on a web viewer.
// Minimum 8 characters; existing sessions are revoked. ESP
// viewers (no password concept) get 400.
func (s *Server) handleAdminViewerPassword(w http.ResponseWriter, r *http.Request) {
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
		s.log.Error("password get viewer", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if info.Type != viewermanager.TypeWeb {
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

// adminViewerTokenResponse is the response from
// /a/viewers/{mac}/regenerate-token. The fresh clear-text token
// is returned ONCE; a second call does not surface the token
// again. The contract is one-shot reveal: token visible once in
// clear, never re-shown.
type adminViewerTokenResponse struct {
	OK        bool   `json:"ok"`
	NewToken  string `json:"new_token"`
	MAC       string `json:"mac"`
}

// handleAdminViewerRegenerateToken creates a fresh bearer token
// for an ESP viewer and returns the clear-text value once. The
// existing handleAdminESPViewersRegenerateToken only returns a
// preview and parks the token in esp_pending_devices for the
// ESP pickup; this endpoint returns the full clear-text so the
// admin UI can show it directly. Both paths coexist: this one
// hands the value to the admin, the older handoff slot stays
// available for the ESP status poll.
func (s *Server) handleAdminViewerRegenerateToken(w http.ResponseWriter, r *http.Request) {
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
		s.log.Error("token regen get viewer", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if info.Type != viewermanager.TypeESP && info.Type != viewermanager.TypeAndroid {
		http.Error(w, "Token-Regeneration nur fuer ESP- und Android-Viewer.", http.StatusBadRequest)
		return
	}
	clearText, hash, err := esptoken.Generate()
	if err != nil {
		s.log.Error("admin token regen generate", "err", err)
		http.Error(w, "Token-Erzeugung fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	if err := s.viewerMgr.SetDeviceTokenHash(r.Context(), mac, hash); err != nil {
		s.log.Error("admin token regen store", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "Token-Speicherung fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	// Also park the token in the pending-handoff slot so a fresh
	// status poll from a not-yet-updated ESP can pick it up
	// automatically. Runs in parallel to the classic
	// /a/esp-viewers/{mac}/regenerate-token path. ESP-only: Android
	// receives its token directly (no esp_pending_devices poll).
	if info.Type == viewermanager.TypeESP {
		s.parkTokenForHandoff(r.Context(), mac, clearText)
	}

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
func adminViewerJSON(info *viewermanager.ViewerInfo) map[string]any {
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
		"has_esp_token":             info.HasDeviceToken,
	}
}

// respondStammdatenErr maps viewermanager errors to the German
// 4xx/500 responses. Mirrors respondSettingsErr.
func (s *Server) respondStammdatenErr(w http.ResponseWriter, mac, field string, err error) {
	if errors.Is(err, viewermanager.ErrViewerNotFound) {
		http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
		return
	}
	s.log.Error("admin stammdaten save",
		"field", field, "err", err, "mac_prefix", safePrefix(mac))
	http.Error(w, "Speichern fehlgeschlagen.", http.StatusInternalServerError)
}
