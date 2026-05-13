package httpserver

import (
	"errors"
	"net/http"
	"strings"

	"unifix.local/server/internal/auth/admin"
	"unifix.local/server/internal/auth/adminsession"
)

// adminLoginPageData matches the Claude-Design admin-login.html
// snippet contract. It carries an optional Error string and a
// CSRFToken slot we keep present even though our auth pipeline
// has no CSRF middleware yet (S13-03+ work).
type adminLoginPageData struct {
	Error     string
	CSRFToken string
}

// handleAdminLoginGet renders the library login form.
//
// Saison 13-02-FIX3: the previous separate "first-run setup"
// page is gone (the library template has no setup form). When
// no admin exists yet, the very first successful POST creates
// the admin with the typed password. UX is simpler and matches
// the library design; the operator should pick a strong password
// at first launch.
func (s *Server) handleAdminLoginGet(w http.ResponseWriter, r *http.Request) {
	if sid := readAdminSessionCookie(r); sid != "" {
		if _, err := s.adminSessions.Validate(r.Context(), sid); err == nil {
			http.Redirect(w, r, "/a/", http.StatusSeeOther)
			return
		}
	}
	s.renderAdminPage(w, "login", adminLoginPageData{})
}

// handleAdminLoginPost handles login (and first-run setup as a
// silent side-effect when no admin exists yet).
func (s *Server) handleAdminLoginPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(r.PostForm.Get("username"))
	password := r.PostForm.Get("password")
	if username == "" || password == "" {
		s.renderAdminPage(w, "login", adminLoginPageData{
			Error: "Benutzername und Passwort sind Pflicht.",
		})
		return
	}

	exists, err := s.admin.Exists(r.Context())
	if err != nil {
		s.log.Error("admin exists check failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// First-run: create the admin from this first valid POST.
	if !exists {
		if err := s.admin.SetPassword(r.Context(), username, password); err != nil {
			s.log.Warn("admin setup rejected", "err", err)
			s.renderAdminPage(w, "login", adminLoginPageData{
				Error: friendlyAdminError(err),
			})
			return
		}
		s.log.Info("admin setup complete", "username", username)
	}

	if err := s.admin.Login(r.Context(), username, password); err != nil {
		if errors.Is(err, admin.ErrNotFound) || errors.Is(err, admin.ErrInvalidPassword) {
			s.renderAdminPage(w, "login", adminLoginPageData{
				Error: "Anmeldedaten ungueltig.",
			})
			return
		}
		s.log.Error("admin login failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	sid, err := s.adminSessions.Create(r.Context(), username, adminsession.Meta{
		UserAgent: r.UserAgent(),
		IP:        clientIP(r),
	})
	if err != nil {
		s.log.Error("admin session create failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.setAdminSessionCookie(w, sid)
	http.Redirect(w, r, "/a/", http.StatusSeeOther)
}

func friendlyAdminError(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, ": "); i >= 0 && i < len(msg)-2 {
		msg = msg[i+2:]
	}
	return msg
}

// renderAdminPage is a small helper that writes the right
// Content-Type and forwards to the template engine.
func (s *Server) renderAdminPage(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.renderPage(w, name, data); err != nil {
		s.log.Error("render page", "name", name, "err", err)
	}
}

func (s *Server) renderAdminPartial(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.renderPartial(w, name, data); err != nil {
		s.log.Error("render partial", "name", name, "err", err)
	}
}
