package httpserver

import (
	"net"
	"net/http"
	"strings"

	"carvilon.local/server/internal/auth/argon2id"
	"carvilon.local/server/internal/auth/loginaudit"
	"carvilon.local/server/internal/auth/ratelimit"
	"carvilon.local/server/internal/auth/session"
	"carvilon.local/server/internal/viewermanager"
	"carvilon.local/server/internal/platformconfig"
)

// viewerLoginPageData is the payload for the login form. Pre-fill
// via URL parameters was removed (security anti-pattern; passwords
// must not land in server logs, browser history or referer).
type viewerLoginPageData struct {
	Username     string
	ErrorMessage string
	Locked       bool
}

// handleLoginGet answers GET /login (the replacement for the
// legacy /einloggen entry point).
//
// With a valid session: 303 redirect to /webviewer/ - the browser
// URL stays clean (no /login with a rendered home body).
// Without a session: render the login form.
//
// ?u= and ?p= URL parameters are IGNORED (the QR-code pre-fill
// path was removed).
func (s *Server) handleLoginGet(w http.ResponseWriter, r *http.Request) {
	if sid := s.readSessionCookie(r); sid != "" {
		if _, err := s.sessions.Validate(r.Context(), sid); err == nil {
			http.Redirect(w, r, "/webviewer/", http.StatusSeeOther)
			return
		}
	}
	s.renderViewerLogin(w, viewerLoginPageData{})
}

// handleViewerLoginPost validates username + password, checks the
// rate limiter and creates a viewer_session on success.
//
// Structured slog logs on every path; the username lookup is
// case-insensitive (DB stores lowercase, mieter may type in
// MixedCase). The QR-code pre-fill fields are no longer accepted.
func (s *Server) handleViewerLoginPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "ungueltige Formular-Daten", http.StatusBadRequest)
		return
	}
	// The form field is still named "username" (browser autofill
	// keys off the HTML attribute) but its content is now the
	// Wohnungs-Name. The audit log and limiter bucket use the
	// normalised value as the stable key.
	nameRaw := strings.TrimSpace(r.PostForm.Get("username"))
	lookupKey := viewermanager.NormalizeName(nameRaw)
	password := r.PostForm.Get("password")
	ip := clientIP(r)
	ua := r.UserAgent()

	log := s.log.With(
		"event", "viewer_login",
		"name", nameRaw,
		"name_lookup", lookupKey,
		"ip", ip,
	)
	log.Info("attempt")

	if nameRaw == "" || password == "" {
		log.Info("rejected", "reason", "missing_field")
		s.renderViewerLogin(w, viewerLoginPageData{
			Username:     nameRaw,
			ErrorMessage: "Wohnungs-Name und Passwort sind Pflicht.",
		})
		return
	}

	dec := s.viewerLimiter.Allow(ip, lookupKey)
	if dec != ratelimit.Allow {
		log.Warn("blocked", "reason", "rate_limit", "decision", dec)
		s.recordAudit(r, loginaudit.Entry{
			Realm:     loginaudit.RealmViewer,
			Username:  nameRaw,
			IP:        ip,
			UserAgent: ua,
			Outcome:   loginaudit.OutcomeLocked,
		})
		s.renderViewerLogin(w, viewerLoginPageData{
			Username: nameRaw,
			Locked:   true,
		})
		return
	}

	info, hash, err := s.viewerMgr.LookupByName(r.Context(), nameRaw)
	if err != nil || hash == "" {
		s.viewerLimiter.RegisterFailure(ip, lookupKey)
		reason := "viewer_not_found"
		if err == nil && hash == "" {
			reason = "no_password_set"
		}
		log.Info("denied", "reason", reason, "lookup_err", err)
		s.recordAudit(r, loginaudit.Entry{
			Realm:     loginaudit.RealmViewer,
			Username:  nameRaw,
			IP:        ip,
			UserAgent: ua,
			Outcome:   loginaudit.OutcomeFail,
		})
		s.renderViewerLogin(w, viewerLoginPageData{
			Username:     nameRaw,
			ErrorMessage: "Falscher Name oder Passwort.",
		})
		return
	}

	pepper, perr := s.platformCfg.GetSecret(r.Context(), platformconfig.KeyViewerPwPepper)
	if perr != nil {
		log.Warn("pepper lookup failed", "err", perr)
	}
	ok, verr := argon2id.VerifyWithPepper(password, pepper, hash)
	if verr != nil || !ok {
		s.viewerLimiter.RegisterFailure(ip, lookupKey)
		log.Info("denied", "reason", "argon2_verify_failed",
			"verify_ok", ok, "verify_err", verr,
			"viewer_mac", info.MAC,
		)
		s.recordAudit(r, loginaudit.Entry{
			Realm:     loginaudit.RealmViewer,
			Username:  nameRaw,
			ViewerMAC: info.MAC,
			IP:        ip,
			UserAgent: ua,
			Outcome:   loginaudit.OutcomeFail,
		})
		s.renderViewerLogin(w, viewerLoginPageData{
			Username:     nameRaw,
			ErrorMessage: "Falscher Name oder Passwort.",
		})
		return
	}

	s.viewerLimiter.RegisterSuccess(lookupKey)

	// Drop any existing session from the same browser so there is
	// no duplicate entry. Other sessions of the same viewer
	// (tablet in the hallway, phone in the pocket) stay.
	if oldSID := s.readSessionCookie(r); oldSID != "" {
		_ = s.sessions.Revoke(r.Context(), oldSID)
	}

	sid, err := s.sessions.Create(r.Context(), info.MAC, session.Meta{
		UserAgent: ua,
		IP:        ip,
	})
	if err != nil {
		log.Error("session create failed", "err", err, "viewer_mac", info.MAC)
		http.Error(w, "Login fehlgeschlagen", http.StatusInternalServerError)
		return
	}

	s.recordAudit(r, loginaudit.Entry{
		Realm:     loginaudit.RealmViewer,
		Username:  nameRaw,
		ViewerMAC: info.MAC,
		IP:        ip,
		UserAgent: ua,
		Outcome:   loginaudit.OutcomeSuccess,
	})

	// IMPORTANT: Set-Cookie MUST go out before http.Redirect or
	// the redirect writes the response status first and our
	// cookie header ends up on the wrong side of the status code.
	s.setSessionCookie(w, sid)
	log.Info("granted",
		"viewer_mac", info.MAC,
		"session_prefix", sidPrefix(sid),
		"cookie_name", s.viewerCookieName(),
		"cookie_path", s.viewerCookiePath(),
		"cookie_secure", !s.cfg.DevMode,
	)
	http.Redirect(w, r, "/webviewer/", http.StatusSeeOther)
}

// handleViewerLogout revokes the viewer session and clears the
// cookie.
func (s *Server) handleViewerLogout(w http.ResponseWriter, r *http.Request) {
	if sid := s.readSessionCookie(r); sid != "" {
		_ = s.sessions.Revoke(r.Context(), sid)
	}
	s.clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) renderViewerLogin(w http.ResponseWriter, data viewerLoginPageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := s.tpl.renderViewer(w, "login", data); err != nil {
		s.log.Error("render viewer login", "err", err)
	}
}

// recordAudit is a convenience wrapper that logs audit errors
// non-fatally. No-op when the loginaudit service is missing.
func (s *Server) recordAudit(r *http.Request, e loginaudit.Entry) {
	if s.audit == nil {
		return
	}
	if err := s.audit.Insert(r.Context(), e); err != nil {
		s.log.Warn("login_audit insert failed", "err", err)
	}
}

// sidPrefix returns the first 8 characters of a session ID;
// enough for a log correlation token without leaking the full
// ID.
func sidPrefix(sid string) string {
	if len(sid) < 8 {
		return sid
	}
	return sid[:8]
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
