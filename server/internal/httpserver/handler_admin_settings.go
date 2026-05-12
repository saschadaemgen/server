package httpserver

import (
	"net/http"
	"strings"

	"unifix.local/server/internal/platformconfig"
	"unifix.local/server/internal/uaapi"
)

type adminSettingsData struct {
	Title     string
	ShowNav   bool
	BaseURL   string
	TokenSet  bool
	Flash     string
	FlashType string // "green" / "red" / "amber"
}

func (s *Server) handleAdminSettingsGet(w http.ResponseWriter, r *http.Request) {
	data := s.buildSettingsData(r)
	s.renderAdminPage(w, "settings", data)
}

func (s *Server) handleAdminSettingsPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	action := r.PostForm.Get("action")
	baseURL := strings.TrimSpace(r.PostForm.Get("base_url"))
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

	// Always re-build the UA client from latest config so subsequent
	// requests see the new credentials immediately.
	storedURL, _ := s.platformCfg.Get(r.Context(), platformconfig.KeyUAAPIBaseURL)
	storedToken, _ := s.platformCfg.GetSecret(r.Context(), platformconfig.KeyUAAPIToken)
	if storedURL != "" && storedToken != "" {
		s.ua = uaapi.New(uaapi.Options{BaseURL: storedURL, Token: storedToken})
	}

	data := s.buildSettingsData(r)
	switch action {
	case "test":
		if s.ua == nil {
			data.Flash = "Base URL und Token muessen gesetzt sein, bevor getestet werden kann."
			data.FlashType = "amber"
		} else if err := s.ua.TestConnection(r.Context()); err != nil {
			s.log.Warn("ua test connection failed", "err", err)
			data.Flash = "Verbindung fehlgeschlagen: " + err.Error()
			data.FlashType = "red"
		} else {
			data.Flash = "Gespeichert. Verbindung zur UA-API erfolgreich."
			data.FlashType = "green"
		}
	default:
		data.Flash = "Gespeichert."
		data.FlashType = "green"
	}
	s.renderAdminPage(w, "settings", data)
}

func (s *Server) buildSettingsData(r *http.Request) adminSettingsData {
	baseURL, _ := s.platformCfg.Get(r.Context(), platformconfig.KeyUAAPIBaseURL)
	tokenEnc, _ := s.platformCfg.GetSecret(r.Context(), platformconfig.KeyUAAPIToken)
	return adminSettingsData{
		Title:    "Einstellungen",
		ShowNav:  true,
		BaseURL:  baseURL,
		TokenSet: tokenEnc != "",
	}
}
