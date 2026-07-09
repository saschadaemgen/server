package httpserver

import (
	"errors"
	"net/http"
	"strings"

	"carvilon.local/server/internal/auth/admin"
	"carvilon.local/server/internal/auth/adminsession"
	"carvilon.local/server/internal/auth/loginaudit"
	"carvilon.local/server/internal/auth/ratelimit"
)

// adminLoginPageData carries an optional Error string and a
// CSRFToken slot. CSRF middleware is not wired yet; the slot
// stays present so the template stays stable when it lands.
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
// silent side-effect when no admin exists yet). Rate-limit and
// login-audit run inline here; hashing flows through the admin
// service (Argon2id default, bcrypt rehash on first login).
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
				Realm:     loginaudit.RealmAdmin,
				Username:  username,
				IP:        ip,
				UserAgent: r.UserAgent(),
				Outcome:   loginaudit.OutcomeLocked,
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
				Realm:     loginaudit.RealmAdmin,
				Username:  username,
				IP:        ip,
				UserAgent: r.UserAgent(),
				Outcome:   loginaudit.OutcomeFail,
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
		Realm:     loginaudit.RealmAdmin,
		Username:  username,
		IP:        ip,
		UserAgent: r.UserAgent(),
		Outcome:   loginaudit.OutcomeSuccess,
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
		ActiveNav:   navSlotFor(name),
		User:        extractUser(data),
		AccentColor: s.resolveAccentColor(),
		Page:        data,
	}
	if err := s.tpl.renderPage(w, name, envelope); err != nil {
		s.log.Error("render page", "name", name, "err", err)
	}
}

// navEnvelope is the outer wrapper every page template receives:
// ActiveNav for the navigation, User for the user-bubble, Page
// for the body. Templates reference the body as .Page.X.
type navEnvelope struct {
	ActiveNav string
	User      adminUser
	// AccentColor is the resolved admin accent (hex "#rrggbb"),
	// injected as a :root{--accent;--color-accent} override by the
	// nav/layout so the whole admin reflects the chosen color.
	AccentColor string
	Page        any
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
	case "android-viewers":
		return "android-viewers"
	case "streams":
		return "streams"
	case "turn":
		return "turn"
	case "designer":
		return "designer"
	case "ua":
		return "ua"
	case "mqtt":
		return "mqtt"
	case "mqtt-monitor":
		return "mqtt-monitor"
	case "telegram":
		return "telegram"
	case "settings":
		return "settings"
	case "viewer-detail":
		// The detail page does not belong to a single nav slot -
		// it is the drill-down view from web-viewers / esp-viewers.
		// We mark no slot; the back-link in the header brings the
		// admin back to the list.
		return ""
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
	case adminAndroidViewersData:
		return v.User
	case adminStreamsData:
		return v.User
	case streamsPageData:
		return v.User
	case adminStreamEditData:
		return v.User
	case adminViewerDetailData:
		return v.User
	case turnPageData:
		return v.User
	case designerData:
		return v.User
	case mqttPageData:
		return v.User
	case telegramPageData:
		return v.User
	case uaOverviewData:
		return v.User
	case placeholderData:
		return v.User
	case adminLoginPageData:
		return adminUser{}
	default:
		return adminUser{}
	}
}
