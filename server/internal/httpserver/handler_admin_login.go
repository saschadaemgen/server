package httpserver

import (
	"errors"
	"net/http"
	"strings"

	"unifix.local/server/internal/auth/admin"
	"unifix.local/server/internal/auth/adminsession"
	"unifix.local/server/internal/auth/loginaudit"
	"unifix.local/server/internal/auth/ratelimit"
)

// adminLoginPageData matches the Claude-Design admin-login.html
// snippet contract. It carries an optional Error string and a
// CSRFToken slot we keep present even though our auth pipeline
// has no CSRF middleware yet (later season work).
type adminLoginPageData struct {
	Error     string
	CSRFToken string
}

func (s *Server) handleAdminLoginGet(w http.ResponseWriter, r *http.Request) {
	if sid := s.readAdminSessionCookie(r); sid != "" {
		if _, err := s.adminSessions.Validate(r.Context(), sid); err == nil {
			http.Redirect(w, r, "/a/", http.StatusSeeOther)
			return
		}
	}
	s.renderAdminPage(w, "login", adminLoginPageData{})
}

// handleAdminLoginPost handles login (and first-run setup as a
// silent side-effect when no admin exists yet). Saison 13-02-FIX4-a
// fuegt Rate-Limit + Login-Audit hinzu; das Hashing fliesst durch
// den admin-Service (Argon2id-Default, bcrypt-Rehash beim ersten
// Login).
func (s *Server) handleAdminLoginPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(r.PostForm.Get("username"))
	password := r.PostForm.Get("password")
	ip := clientIP(r)

	if username == "" || password == "" {
		s.renderAdminPage(w, "login", adminLoginPageData{
			Error: "Benutzername und Passwort sind Pflicht.",
		})
		return
	}

	if s.adminLimiter != nil {
		dec := s.adminLimiter.Allow(ip, username)
		if dec != ratelimit.Allow {
			s.recordAudit(r, loginaudit.Entry{
				Realm:    loginaudit.RealmAdmin,
				Username: username,
				IP:       ip,
				UserAgent: r.UserAgent(),
				Outcome:  loginaudit.OutcomeLocked,
			})
			s.renderAdminPage(w, "login", adminLoginPageData{
				Error: "Zu viele fehlgeschlagene Versuche. Bitte spaeter erneut versuchen.",
			})
			return
		}
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
			if s.adminLimiter != nil {
				s.adminLimiter.RegisterFailure(ip, username)
			}
			s.recordAudit(r, loginaudit.Entry{
				Realm:    loginaudit.RealmAdmin,
				Username: username,
				IP:       ip,
				UserAgent: r.UserAgent(),
				Outcome:  loginaudit.OutcomeFail,
			})
			s.renderAdminPage(w, "login", adminLoginPageData{
				Error: "Anmeldedaten ungueltig.",
			})
			return
		}
		s.log.Error("admin login failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if s.adminLimiter != nil {
		s.adminLimiter.RegisterSuccess(username)
	}

	sid, err := s.adminSessions.Create(r.Context(), username, adminsession.Meta{
		UserAgent: r.UserAgent(),
		IP:        ip,
	})
	if err != nil {
		s.log.Error("admin session create failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.recordAudit(r, loginaudit.Entry{
		Realm:    loginaudit.RealmAdmin,
		Username: username,
		IP:       ip,
		UserAgent: r.UserAgent(),
		Outcome:  loginaudit.OutcomeSuccess,
	})
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
// Content-Type and forwards to the template engine. It wraps
// the page-specific data inside an envelope that carries the
// active nav slot so the shared {{template "admin-nav" .}} can
// highlight the current entry.
func (s *Server) renderAdminPage(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	envelope := navEnvelope{
		ActiveNav: navSlotFor(name),
		User:      extractUser(data),
		Page:      data,
	}
	if err := s.tpl.renderPage(w, name, envelope); err != nil {
		s.log.Error("render page", "name", name, "err", err)
	}
}

// navEnvelope ist die aeussere Hulle, die jedes Page-Template
// bekommt: ActiveNav fuer die Navigation, User fuer die User-Bubble,
// Page fuer den Body. Templates referenzieren den Body als .Page.X.
type navEnvelope struct {
	ActiveNav string
	User      adminUser
	Page      any
}

func navSlotFor(name string) string {
	switch name {
	case "dashboard":
		return "dashboard"
	case "web-viewers":
		return "web-viewers"
	case "esp-viewers":
		return "esp-viewers"
	case "users", "user-detail":
		return "users"
	case "esp-pager":
		return "esp-pager"
	case "streams", "stream-edit":
		return "streams"
	case "settings":
		return "settings"
	default:
		return ""
	}
}

// extractUser uses reflection-light type-switching to pull the
// .User field out of every page-data struct. The handlers all
// build a user-slot anyway; this lets the nav template see it
// without the page having to spell it twice.
func extractUser(data any) adminUser {
	switch v := data.(type) {
	case adminDashboardData:
		return v.User
	case adminSettingsData:
		return v.User
	case adminWebViewersData:
		return v.User
	case adminUsersData:
		return v.User
	case adminUserDetailData:
		return v.User
	case adminESPViewersData:
		return v.User
	case adminStreamsData:
		return v.User
	case adminStreamEditData:
		return v.User
	case placeholderData:
		return v.User
	case adminLoginPageData:
		return adminUser{}
	default:
		return adminUser{}
	}
}
