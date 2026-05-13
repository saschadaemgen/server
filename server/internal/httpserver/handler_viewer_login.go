package httpserver

import (
	"context"
	"net"
	"net/http"
	"strings"

	"unifix.local/server/internal/auth/argon2id"
	"unifix.local/server/internal/auth/loginaudit"
	"unifix.local/server/internal/auth/ratelimit"
	"unifix.local/server/internal/auth/session"
)

// viewerLoginPageData ist die Payload fuer das Login-Form.
type viewerLoginPageData struct {
	Username   string
	Error      string
	BlockedMsg string
	PrefillPw  string // optional, via ?p= URL-Parameter (QR-Auto-Fill)
}

// handleViewerRoot beantwortet GET /m und GET /m/.
//
// Mit gueltiger Session: Forward an handleHome.
// Ohne Session: Login-Form anzeigen, ggf. mit URL-Pre-Fill aus
// ?u= und ?p= (QR-Code-Pfad).
func (s *Server) handleViewerRoot(w http.ResponseWriter, r *http.Request) {
	if sid := s.readSessionCookie(r); sid != "" {
		if mac, err := s.sessions.Validate(r.Context(), sid); err == nil {
			ctx := context.WithValue(r.Context(), ctxKeyViewerMAC, mac)
			s.handleHome(w, r.WithContext(ctx))
			return
		}
	}
	q := r.URL.Query()
	s.renderViewerLogin(w, viewerLoginPageData{
		Username:  q.Get("u"),
		PrefillPw: q.Get("p"),
	})
}

// handleViewerLoginPost validiert Username+Passwort, prueft den
// Rate-Limiter und legt bei Erfolg eine viewer_session an.
func (s *Server) handleViewerLoginPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "ungueltige Formular-Daten", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(r.PostForm.Get("username"))
	password := r.PostForm.Get("password")
	ip := clientIP(r)

	if username == "" || password == "" {
		s.renderViewerLogin(w, viewerLoginPageData{
			Username: username,
			Error:    "Benutzername und Passwort sind Pflicht.",
		})
		return
	}

	dec := s.viewerLimiter.Allow(ip, username)
	if dec != ratelimit.Allow {
		s.recordAudit(r, loginaudit.Entry{
			Realm:    loginaudit.RealmViewer,
			Username: username,
			IP:       ip,
			UserAgent: r.UserAgent(),
			Outcome:  loginaudit.OutcomeLocked,
		})
		s.renderViewerLogin(w, viewerLoginPageData{
			Username:   username,
			BlockedMsg: "Zu viele Versuche. Bitte spaeter erneut versuchen oder beim Hausverwalter Bescheid geben.",
		})
		return
	}

	info, hash, err := s.mockMgr.LookupByUsername(r.Context(), username)
	if err != nil || hash == "" {
		s.viewerLimiter.RegisterFailure(ip, username)
		s.recordAudit(r, loginaudit.Entry{
			Realm:    loginaudit.RealmViewer,
			Username: username,
			IP:       ip,
			UserAgent: r.UserAgent(),
			Outcome:  loginaudit.OutcomeFail,
		})
		s.renderViewerLogin(w, viewerLoginPageData{
			Username: username,
			Error:    "Falscher Benutzername oder Passwort.",
		})
		return
	}

	pepper, _ := s.platformCfg.GetSecret(r.Context(), pepperKey())
	ok, err := argon2id.VerifyWithPepper(password, pepper, hash)
	if err != nil || !ok {
		s.viewerLimiter.RegisterFailure(ip, username)
		s.recordAudit(r, loginaudit.Entry{
			Realm:     loginaudit.RealmViewer,
			Username:  username,
			ViewerMAC: info.MAC,
			IP:        ip,
			UserAgent: r.UserAgent(),
			Outcome:   loginaudit.OutcomeFail,
		})
		s.renderViewerLogin(w, viewerLoginPageData{
			Username: username,
			Error:    "Falscher Benutzername oder Passwort.",
		})
		return
	}

	s.viewerLimiter.RegisterSuccess(username)

	// Bestehende Sessions desselben Cookies werden ungueltig
	// gemacht. Andere Sessions des Viewers bleiben (mehrere
	// Geraete pro Wohnung erlaubt).
	if oldSID := s.readSessionCookie(r); oldSID != "" {
		_ = s.sessions.Revoke(r.Context(), oldSID)
	}

	sid, err := s.sessions.Create(r.Context(), info.MAC, session.Meta{
		UserAgent: r.UserAgent(),
		IP:        ip,
	})
	if err != nil {
		s.log.Error("viewer session create", "err", err)
		http.Error(w, "Login fehlgeschlagen", http.StatusInternalServerError)
		return
	}

	s.recordAudit(r, loginaudit.Entry{
		Realm:     loginaudit.RealmViewer,
		Username:  username,
		ViewerMAC: info.MAC,
		IP:        ip,
		UserAgent: r.UserAgent(),
		Outcome:   loginaudit.OutcomeSuccess,
	})

	s.setSessionCookie(w, sid)
	http.Redirect(w, r, "/m", http.StatusSeeOther)
}

// handleViewerLogout revokiert die Viewer-Session und loescht das
// Cookie.
func (s *Server) handleViewerLogout(w http.ResponseWriter, r *http.Request) {
	if sid := s.readSessionCookie(r); sid != "" {
		_ = s.sessions.Revoke(r.Context(), sid)
	}
	s.clearSessionCookie(w)
	http.Redirect(w, r, "/m", http.StatusSeeOther)
}

func (s *Server) renderViewerLogin(w http.ResponseWriter, data viewerLoginPageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := s.tpl.renderViewer(w, "login", data); err != nil {
		s.log.Error("render viewer login", "err", err)
	}
}

// recordAudit ist ein Convenience-Wrapper der Audit-Fehler
// nicht-fatal logged. Wenn der loginaudit-Service fehlt, no-op.
func (s *Server) recordAudit(r *http.Request, e loginaudit.Entry) {
	if s.audit == nil {
		return
	}
	if err := s.audit.Insert(r.Context(), e); err != nil {
		s.log.Warn("login_audit insert failed", "err", err)
	}
}

// pepperKey ist die platform_config-Konstante - Indirection wegen
// Import-Loop-Schutz (platformconfig importiert nicht httpserver).
func pepperKey() string {
	return "viewer_pw_pepper"
}

// clientIP strips the port from r.RemoteAddr. Falls back to the
// raw value if SplitHostPort fails (e.g. when a test injects a
// bare address).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

