package httpserver

import (
	"net/http"
	"strings"

	"unifix.local/server/internal/platformconfig"
	"unifix.local/server/internal/uaapi"
)

// adminSettingsData carries the data the Claude-Design admin-
// settings.html snippet expects: User + Settings + CSRFToken.
type adminSettingsData struct {
	User      adminUser
	Settings  adminSettingsBag
	CSRFToken string
	Flash     string
	FlashType string // "green" | "red" | "amber"
}

type adminSettingsBag struct {
	OrgName           string
	TimeZone          string
	UAControllerURL   string
	UAStatus          string // "connected" | "error" | "untested"
	UALastSync        string
	MagicLinkLifetime string // "24h" | "7d" | "30d" | "never"
	MagicLinkRenew    bool
	SMTPHost          string
	SMTPFrom          string
	AdminTheme        string // "dark" | "light" | "system"
}

func (s *Server) handleAdminSettingsGet(w http.ResponseWriter, r *http.Request) {
	s.renderAdminPage(w, "settings", s.buildSettingsData(r))
}

func (s *Server) handleAdminSettingsPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	// The library form posts a number of fields; right now we
	// persist the UA controller URL + token (existing platform_-
	// config behaviour). The other settings (Org, SMTP, theme...)
	// are accepted but not yet persisted - that lands in a
	// follow-up saison once we have a real settings table.
	baseURL := strings.TrimSpace(r.PostForm.Get("ua_controller_url"))
	if baseURL == "" {
		// fallback to legacy field name for backwards-compat
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

	// Re-build the UA client so the next request sees the new credentials.
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

func (s *Server) buildSettingsData(r *http.Request) adminSettingsData {
	username := AdminUserFromContext(r.Context())
	baseURL, _ := s.platformCfg.Get(r.Context(), platformconfig.KeyUAAPIBaseURL)
	tokenEnc, _ := s.platformCfg.GetSecret(r.Context(), platformconfig.KeyUAAPIToken)

	status := "untested"
	if baseURL != "" && tokenEnc != "" {
		status = "connected"
	} else if baseURL != "" || tokenEnc != "" {
		status = "untested"
	}
	return adminSettingsData{
		User: adminUser{Name: username, Initials: initialsOf(username)},
		Settings: adminSettingsBag{
			OrgName:           "unifix",
			TimeZone:          "Europe/Berlin",
			UAControllerURL:   baseURL,
			UAStatus:          status,
			MagicLinkLifetime: "24h",
			MagicLinkRenew:    false,
			AdminTheme:        "dark",
		},
	}
}
