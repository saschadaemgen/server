package httpserver

import (
	"errors"
	"net/http"
	"strings"

	"unifix.local/server/internal/auth/admin"
	"unifix.local/server/internal/auth/adminsession"
)

type adminLoginPageData struct {
	Title    string
	ShowNav  bool
	Setup    bool
	Username string
	Error    string
}

// handleAdminLoginGet renders the login form. If no admin user
// exists yet (first-run), the form switches into setup mode
// (asks for password confirm).
func (s *Server) handleAdminLoginGet(w http.ResponseWriter, r *http.Request) {
	// already logged in? skip to dashboard.
	if sid := readAdminSessionCookie(r); sid != "" {
		if _, err := s.adminSessions.Validate(r.Context(), sid); err == nil {
			http.Redirect(w, r, "/a/", http.StatusSeeOther)
			return
		}
	}
	exists, err := s.admin.Exists(r.Context())
	if err != nil {
		s.log.Error("admin exists check failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data := adminLoginPageData{
		Title:   "Login",
		ShowNav: false,
		Setup:   !exists,
	}
	s.renderAdminPage(w, "login", data)
}

// handleAdminLoginPost handles either setup or login depending
// on form contents.
func (s *Server) handleAdminLoginPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(r.PostForm.Get("username"))
	password := r.PostForm.Get("password")
	isSetup := r.PostForm.Get("setup") == "1"

	exists, err := s.admin.Exists(r.Context())
	if err != nil {
		s.log.Error("admin exists check failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Setup-Pfad: trat ein wenn noch kein Admin existiert.
	if !exists {
		if !isSetup {
			http.Redirect(w, r, "/a/login", http.StatusSeeOther)
			return
		}
		confirm := r.PostForm.Get("password_confirm")
		if password == "" || password != confirm {
			s.renderAdminPage(w, "login", adminLoginPageData{
				Title: "Setup", Setup: true, Username: username,
				Error: "Passwoerter stimmen nicht ueberein oder sind leer.",
			})
			return
		}
		if err := s.admin.SetPassword(r.Context(), username, password); err != nil {
			s.log.Warn("admin setup rejected", "err", err)
			s.renderAdminPage(w, "login", adminLoginPageData{
				Title: "Setup", Setup: true, Username: username,
				Error: friendlyAdminError(err),
			})
			return
		}
		s.log.Info("admin setup complete", "username", username)
	}

	// Login-Pfad (auch direkt nach Setup).
	if err := s.admin.Login(r.Context(), username, password); err != nil {
		if errors.Is(err, admin.ErrNotFound) || errors.Is(err, admin.ErrInvalidPassword) {
			s.renderAdminPage(w, "login", adminLoginPageData{
				Title: "Login", Username: username,
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
