package httpserver

import (
	"net/http"
	"strings"
	"time"

	"unifix.local/server/internal/auth/loginaudit"
	"unifix.local/server/internal/platformconfig"
	"unifix.local/server/internal/uaapi"
)

// adminSettingsData ist die Payload fuer das eigene Settings-
// Template (kein Library-Snippet mehr; das Library-Template hat
// Fake-Sektionen die wir bewusst weglassen).
type adminSettingsData struct {
	User     adminUser
	UA       uaSettingsBlock
	Audit    []auditRow
	Locks    []lockRow
	Flash    string
	FlashType string
}

type uaSettingsBlock struct {
	BaseURL    string
	Status     string // "connected" | "untested" | "error"
	HasToken   bool
}

type auditRow struct {
	When    string
	Realm   string
	User    string
	IP      string
	Outcome string
}

type lockRow struct {
	Kind        string // "user" | "ip"
	Value       string
	UntilLabel  string
	Attempts    int
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

// handleAdminPasswordPost erlaubt dem eingeloggten Admin sein
// eigenes Passwort zu aendern (alt + neu + bestaetigung).
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

// handleAdminUnlockLock entsperrt eine IP- oder Username-Sperre
// auf der Settings-Seite (POST mit kind=user|ip + value).
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
		Realm:    loginaudit.RealmAdmin,
		Username: value,
		IP:       clientIP(r),
		UserAgent: r.UserAgent(),
		Outcome:  loginaudit.OutcomeUnlocked,
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

	data := adminSettingsData{
		User: adminUser{Name: username, Initials: initialsOf(username)},
		UA: uaSettingsBlock{
			BaseURL:  baseURL,
			Status:   status,
			HasToken: tokenEnc != "",
		},
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

// humanDuration rendert "2 min" / "45 s" - knapp, fuer Listen.
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
