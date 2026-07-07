package httpserver

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"carvilon.local/server/internal/auth/loginaudit"
	"carvilon.local/server/internal/platformconfig"
	"carvilon.local/server/internal/protectapi"
	"carvilon.local/server/internal/uaapi"
)

// adminSettingsData is the payload for our own settings template
// (no library snippet anymore; the library template had fake
// sections we deliberately leave out).
type adminSettingsData struct {
	User      adminUser
	UA        uaSettingsBlock
	Protect   protectSettingsBlock
	Shelly    shellySettingsBlock
	Station   stationSettingsBlock
	Accent    accentSettingsBlock
	Audit     []auditRow
	Locks     []lockRow
	Flash     string
	FlashType string
}

// stationSettingsBlock holds the Open-Meteo coordinates the
// mieter screensaver and the ESP weather card pull from.
// Defaults seed Recklinghausen on a fresh DB.
type stationSettingsBlock struct {
	Lat string
	Lon string
}

type uaSettingsBlock struct {
	BaseURL  string
	Status   string // "connected" | "untested" | "error"
	HasToken bool
	// Enabled ist der effektive "UA aktiv"-Schalter. Steuert nur die
	// Benutzer-Seite (blendet den UA-Abschnitt ein/aus); CARVILONs
	// eigene Benutzer sind davon unberuehrt.
	Enabled bool
}

// protectSettingsBlock mirrors uaSettingsBlock for the UniFi Protect
// Integration (Saison 21 - Protect Etappe 1). Der API-Key selbst
// erreicht die Seite nie - nur HasKey ("gesetzt").
type protectSettingsBlock struct {
	BaseURL string
	HasKey  bool
	Enabled bool
}

// shellySettingsBlock mirrors the pattern for the Shelly integration
// (Saison 21 - Shelly Etappe 1/2). Addresses render back into the form (the
// admin's own MANUAL list, from the device table); the auth password never
// does - only HasPassword ("set"). Etappe 2 adds the discovery summary and
// the sticky ignore list so a removed device can be released again.
type shellySettingsBlock struct {
	Addresses   string
	HasPassword bool
	Enabled     bool

	// DiscoveredCount is how many active devices came from mDNS (not the
	// manual list) - the "found automatically" evidence.
	DiscoveredCount int
	// AutoAdopt is the approval-gate toggle: false (default) = discovered
	// devices wait as pending; true = auto-activate.
	AutoAdopt bool
	// KeepCloud is the "keep Shelly cloud" opt-in used during provisioning:
	// false (default) disables the device cloud connection as hardening.
	KeepCloud bool
	// Pending is the "awaiting approval" list: devices found by discovery
	// while the gate is on. Records only - never polled.
	Pending []shellyPendingRow
	// Ignored is the sticky ignore list: devices manually removed or
	// rejected, each releasable back into discovery.
	Ignored []shellyIgnoredRow
}

// shellyPendingRow is one entry of the "Pending approval" view. It shows only
// what the announcement carried (no poll happened): MAC + address.
type shellyPendingRow struct {
	ID   int64
	MAC  string
	Addr string
}

// shellyIgnoredRow is one entry of the "Ignored devices" view.
type shellyIgnoredRow struct {
	ID    int64
	Label string // MAC when known, else the configured address
	MAC   string
	Addr  string
}

type auditRow struct {
	When    string
	Realm   string
	User    string
	IP      string
	Outcome string
}

type lockRow struct {
	Kind       string // "user" | "ip"
	Value      string
	UntilLabel string
	Attempts   int
}

func (s *Server) handleAdminSettingsGet(w http.ResponseWriter, r *http.Request) {
	s.renderAdminPage(w, "settings", s.buildSettingsData(r))
}

func (s *Server) handleAdminSettingsPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	baseURL := strings.TrimSpace(r.PostForm.Get("ua_controller_url"))
	if baseURL == "" {
		baseURL = strings.TrimSpace(r.PostForm.Get("base_url"))
	}
	token := r.PostForm.Get("token")

	if baseURL != "" {
		if err := s.platformCfg.Set(r.Context(), platformconfig.KeyUAAPIBaseURL, baseURL); err != nil {
			s.log.Error("save base_url failed", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	if token != "" {
		if err := s.platformCfg.SetSecret(r.Context(), platformconfig.KeyUAAPIToken, token); err != nil {
			s.log.Error("save token failed", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}

	// "UA aktiv"-Schalter. Die Checkbox sendet ihren Namen nur wenn
	// angehakt; wir schreiben den Zustand deshalb immer explizit als
	// "1"/"0", damit der Default (Token-abhaengig) danach nicht mehr
	// greift und der Admin-Wille die Wahrheit ist.
	uaEnabledVal := "0"
	if r.PostForm.Get("ua_enabled") != "" {
		uaEnabledVal = "1"
	}
	if err := s.platformCfg.Set(r.Context(), platformconfig.KeyUAEnabled, uaEnabledVal); err != nil {
		s.log.Error("save ua_enabled failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	storedURL, _ := s.platformCfg.Get(r.Context(), platformconfig.KeyUAAPIBaseURL)
	storedToken, _ := s.platformCfg.GetSecret(r.Context(), platformconfig.KeyUAAPIToken)
	if storedURL != "" && storedToken != "" {
		s.ua = uaapi.New(uaapi.Options{BaseURL: storedURL, Token: storedToken})
	}

	data := s.buildSettingsData(r)
	data.Flash = "Gespeichert."
	data.FlashType = "green"
	s.renderAdminPage(w, "settings", data)
}

// handleAdminProtectSettingsPost speichert Host + X-API-KEY + den
// "Protect aktiv"-Schalter der UniFi-Protect-Integration (eigenes
// Formular in /a/settings, Muster wie beim UA-Token). Der Key landet
// AES-256-GCM-verschluesselt in platform_config und wird nie geloggt
// oder zurueckgerendert; danach wird der Client sofort neu gebaut.
func (s *Server) handleAdminProtectSettingsPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	baseURL := strings.TrimSpace(r.PostForm.Get("protect_controller_url"))
	apiKey := r.PostForm.Get("protect_api_key")

	if baseURL != "" {
		if err := s.platformCfg.Set(r.Context(), platformconfig.KeyProtectAPIBaseURL, baseURL); err != nil {
			s.log.Error("save protect base_url failed", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	if apiKey != "" {
		if err := s.platformCfg.SetSecret(r.Context(), platformconfig.KeyProtectAPIKey, apiKey); err != nil {
			s.log.Error("save protect api key failed", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}

	// Wie beim UA-Schalter: die Checkbox sendet ihren Namen nur wenn
	// angehakt; wir schreiben immer explizit "1"/"0", damit der
	// Key-abhaengige Default danach nicht mehr greift.
	enabledVal := "0"
	if r.PostForm.Get("protect_enabled") != "" {
		enabledVal = "1"
	}
	if err := s.platformCfg.Set(r.Context(), platformconfig.KeyProtectEnabled, enabledVal); err != nil {
		s.log.Error("save protect_enabled failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	storedURL, _ := s.platformCfg.Get(r.Context(), platformconfig.KeyProtectAPIBaseURL)
	storedKey, _ := s.platformCfg.GetSecret(r.Context(), platformconfig.KeyProtectAPIKey)
	if storedURL != "" && storedKey != "" {
		s.protect = protectapi.New(protectapi.Options{BaseURL: storedURL, APIKey: storedKey})
	}

	data := s.buildSettingsData(r)
	data.Flash = "Gespeichert."
	data.FlashType = "green"
	s.renderAdminPage(w, "settings", data)
}

// protectEnabled ist der effektive "Protect aktiv"-Schalter, gleiche
// Semantik wie uaEnabled: explizites "1"/"0" gewinnt; fehlt der Wert,
// gilt an-wenn-Key-gesetzt.
func (s *Server) protectEnabled(ctx context.Context) bool {
	if s.platformCfg == nil {
		return false
	}
	switch raw, _ := s.platformCfg.Get(ctx, platformconfig.KeyProtectEnabled); raw {
	case "1":
		return true
	case "0":
		return false
	default:
		key, _ := s.platformCfg.GetSecret(ctx, platformconfig.KeyProtectAPIKey)
		return strings.TrimSpace(key) != ""
	}
}

// handleAdminPasswordPost lets the logged-in admin change their
// own password (old + new + confirmation).
func (s *Server) handleAdminPasswordPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	username := AdminUserFromContext(r.Context())
	old := r.PostForm.Get("old_password")
	neu := r.PostForm.Get("new_password")
	confirm := r.PostForm.Get("confirm_password")

	data := s.buildSettingsData(r)
	if neu == "" || neu != confirm {
		data.Flash = "Neues Passwort und Bestaetigung muessen uebereinstimmen."
		data.FlashType = "red"
		s.renderAdminPage(w, "settings", data)
		return
	}
	if err := s.admin.Login(r.Context(), username, old); err != nil {
		data.Flash = "Altes Passwort falsch."
		data.FlashType = "red"
		s.renderAdminPage(w, "settings", data)
		return
	}
	if err := s.admin.SetPassword(r.Context(), username, neu); err != nil {
		data.Flash = friendlyAdminError(err)
		data.FlashType = "red"
		s.renderAdminPage(w, "settings", data)
		return
	}
	data = s.buildSettingsData(r)
	data.Flash = "Passwort geaendert."
	data.FlashType = "green"
	s.renderAdminPage(w, "settings", data)
}

// handleAdminUnlockLock clears an IP- or username-based lockout
// from the settings page (POST with kind=user|ip + value).
func (s *Server) handleAdminUnlockLock(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	kind := r.PostForm.Get("kind")
	value := r.PostForm.Get("value")
	if value == "" {
		http.Error(w, "value required", http.StatusBadRequest)
		return
	}
	switch kind {
	case "user":
		if s.viewerLimiter != nil {
			s.viewerLimiter.ClearUser(value)
		}
		if s.adminLimiter != nil {
			s.adminLimiter.ClearUser(value)
		}
	case "ip":
		if s.viewerLimiter != nil {
			s.viewerLimiter.ClearIP(value)
		}
		if s.adminLimiter != nil {
			s.adminLimiter.ClearIP(value)
		}
	default:
		http.Error(w, "kind must be 'user' or 'ip'", http.StatusBadRequest)
		return
	}
	s.recordAudit(r, loginaudit.Entry{
		Realm:     loginaudit.RealmAdmin,
		Username:  value,
		IP:        clientIP(r),
		UserAgent: r.UserAgent(),
		Outcome:   loginaudit.OutcomeUnlocked,
	})
	http.Redirect(w, r, "/a/settings", http.StatusSeeOther)
}

func (s *Server) buildSettingsData(r *http.Request) adminSettingsData {
	username := AdminUserFromContext(r.Context())
	baseURL, _ := s.platformCfg.Get(r.Context(), platformconfig.KeyUAAPIBaseURL)
	tokenEnc, _ := s.platformCfg.GetSecret(r.Context(), platformconfig.KeyUAAPIToken)

	status := "untested"
	if baseURL != "" && tokenEnc != "" {
		status = "connected"
	}

	stationLat, _ := s.platformCfg.Get(r.Context(), platformconfig.KeyStationLat)
	stationLon, _ := s.platformCfg.Get(r.Context(), platformconfig.KeyStationLon)

	protectURL, _ := s.platformCfg.Get(r.Context(), platformconfig.KeyProtectAPIBaseURL)
	protectKey, _ := s.platformCfg.GetSecret(r.Context(), platformconfig.KeyProtectAPIKey)

	shellyPw, _ := s.platformCfg.GetSecret(r.Context(), platformconfig.KeyShellyPassword)
	shellyBlock := s.buildShellySettingsBlock(r.Context())
	shellyBlock.HasPassword = shellyPw != ""
	shellyBlock.Enabled = s.shellyEnabled(r.Context())
	shellyBlock.AutoAdopt = s.shellyAutoAdopt(r.Context())
	shellyBlock.KeepCloud = s.shellyKeepCloud(r.Context())

	data := adminSettingsData{
		User: adminUser{Name: username, Initials: initialsOf(username)},
		UA: uaSettingsBlock{
			BaseURL:  baseURL,
			Status:   status,
			HasToken: tokenEnc != "",
			Enabled:  s.uaEnabled(r.Context()),
		},
		Protect: protectSettingsBlock{
			BaseURL: protectURL,
			HasKey:  protectKey != "",
			Enabled: s.protectEnabled(r.Context()),
		},
		Shelly: shellyBlock,
		Station: stationSettingsBlock{
			Lat: stationLat,
			Lon: stationLon,
		},
		Accent: s.accentSettingsBlock(),
	}

	if s.audit != nil {
		entries, err := s.audit.Recent(r.Context(), "", 50)
		if err == nil {
			for _, e := range entries {
				data.Audit = append(data.Audit, auditRow{
					When:    e.Timestamp.Local().Format("02.01.2006 15:04:05"),
					Realm:   string(e.Realm),
					User:    e.Username,
					IP:      e.IP,
					Outcome: string(e.Outcome),
				})
			}
		}
	}

	now := time.Now()
	if s.viewerLimiter != nil {
		for _, l := range s.viewerLimiter.LockedUsers() {
			data.Locks = append(data.Locks, lockRow{
				Kind:       "user",
				Value:      l.Value,
				Attempts:   l.Attempts,
				UntilLabel: humanDuration(l.LockedUntil.Sub(now)),
			})
		}
		for _, l := range s.viewerLimiter.HotIPs(3) {
			data.Locks = append(data.Locks, lockRow{
				Kind:     "ip",
				Value:    l.Value,
				Attempts: l.Attempts,
			})
		}
	}
	if s.adminLimiter != nil {
		for _, l := range s.adminLimiter.LockedUsers() {
			data.Locks = append(data.Locks, lockRow{
				Kind:       "user",
				Value:      l.Value,
				Attempts:   l.Attempts,
				UntilLabel: humanDuration(l.LockedUntil.Sub(now)),
			})
		}
	}
	return data
}

// humanDuration renders "2 min" / "45 s" - terse, for list rows.
func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "abgelaufen"
	}
	if d < time.Minute {
		return secondsLabel(int(d.Seconds()))
	}
	return minutesLabel(int(d.Minutes()))
}

func secondsLabel(n int) string {
	return itoa(n) + " s"
}
func minutesLabel(n int) string {
	return itoa(n) + " min"
}

// handleAdminStationPost saves the operator's site coordinates
// for the weather backend. Values are validated as floats in
// the standard geographic ranges; on parse error the existing
// values stay and the settings page renders a red flash.
func (s *Server) handleAdminStationPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	latStr := strings.TrimSpace(r.PostForm.Get("station_lat"))
	lonStr := strings.TrimSpace(r.PostForm.Get("station_lon"))

	data := s.buildSettingsData(r)
	lat, err := parseLatLon(latStr, -90, 90)
	if err != nil {
		data.Flash = "Breitengrad: " + err.Error()
		data.FlashType = "red"
		s.renderAdminPage(w, "settings", data)
		return
	}
	lon, err := parseLatLon(lonStr, -180, 180)
	if err != nil {
		data.Flash = "Laengengrad: " + err.Error()
		data.FlashType = "red"
		s.renderAdminPage(w, "settings", data)
		return
	}
	// Persist as canonical 4-decimal strings so the saved value
	// matches what open-meteo will round to anyway (~11 m).
	if err := s.platformCfg.Set(r.Context(), platformconfig.KeyStationLat,
		strconv.FormatFloat(lat, 'f', 4, 64)); err != nil {
		s.log.Error("save station_lat", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.platformCfg.Set(r.Context(), platformconfig.KeyStationLon,
		strconv.FormatFloat(lon, 'f', 4, 64)); err != nil {
		s.log.Error("save station_lon", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data = s.buildSettingsData(r)
	data.Flash = "Standort gespeichert. Wirkt beim naechsten Wetter-Refresh (max 15 Minuten)."
	data.FlashType = "green"
	s.renderAdminPage(w, "settings", data)
}

// parseLatLon parses a float and bounds it. The bounds catch
// typos like a swapped lat/lon pair or an extra digit; everything
// else (locale-comma vs dot, whitespace) is the caller's problem.
func parseLatLon(s string, low, high float64) (float64, error) {
	if s == "" {
		return 0, fmt.Errorf("darf nicht leer sein")
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", "."), 64)
	if err != nil {
		return 0, fmt.Errorf("keine gueltige Zahl (%s)", s)
	}
	if v < low || v > high {
		return 0, fmt.Errorf("muss zwischen %.0f und %.0f liegen", low, high)
	}
	return v, nil
}
