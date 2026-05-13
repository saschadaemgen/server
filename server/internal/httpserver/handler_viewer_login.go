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
	"unifix.local/server/internal/platformconfig"
)

// viewerLoginPageData ist die Payload fuer das Login-Form.
// Saison 13-02-FIX4-a-HOTFIX1: Pre-Fill via URL-Parameter ist
// raus (Sicherheits-Anti-Pattern; Passwoerter sollen nicht in
// Server-Logs / Browser-History / Referer landen).
type viewerLoginPageData struct {
	Username     string
	ErrorMessage string
	Locked       bool
}

// handleViewerRoot beantwortet GET /m und GET /m/.
//
// Mit gueltiger Session: Forward an handleHome.
// Ohne Session: Login-Form anzeigen.
//
// Saison 13-02-FIX4-a-HOTFIX1: ?u= und ?p= URL-Parameter werden
// IGNORIERT (Pre-Fill via QR-Code wurde entfernt).
func (s *Server) handleViewerRoot(w http.ResponseWriter, r *http.Request) {
	if sid := s.readSessionCookie(r); sid != "" {
		if mac, err := s.sessions.Validate(r.Context(), sid); err == nil {
			ctx := context.WithValue(r.Context(), ctxKeyViewerMAC, mac)
			s.handleHome(w, r.WithContext(ctx))
			return
		}
	}
	s.renderViewerLogin(w, viewerLoginPageData{})
}

// handleViewerLoginPost validiert Username+Passwort, prueft den
// Rate-Limiter und legt bei Erfolg eine viewer_session an.
//
// Saison 13-02-FIX4-a-HOTFIX1: strukturierte slog-Logs an jedem
// Pfad, case-insensitive Username-Lookup (DB hat Username
// lowercase, Mieter darf auch in MixedCase tippen), QR-Code-
// Pre-Fill-Felder raus.
func (s *Server) handleViewerLoginPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "ungueltige Formular-Daten", http.StatusBadRequest)
		return
	}
	usernameRaw := strings.TrimSpace(r.PostForm.Get("username"))
	username := normalizeUsername(usernameRaw)
	password := r.PostForm.Get("password")
	ip := clientIP(r)
	ua := r.UserAgent()

	log := s.log.With(
		"event", "viewer_login",
		"username", usernameRaw,
		"username_lookup", username,
		"ip", ip,
	)
	log.Info("attempt")

	if usernameRaw == "" || password == "" {
		log.Info("rejected", "reason", "missing_field")
		s.renderViewerLogin(w, viewerLoginPageData{
			Username:     usernameRaw,
			ErrorMessage: "Benutzername und Passwort sind Pflicht.",
		})
		return
	}

	dec := s.viewerLimiter.Allow(ip, username)
	if dec != ratelimit.Allow {
		log.Warn("blocked", "reason", "rate_limit", "decision", dec)
		s.recordAudit(r, loginaudit.Entry{
			Realm:     loginaudit.RealmViewer,
			Username:  usernameRaw,
			IP:        ip,
			UserAgent: ua,
			Outcome:   loginaudit.OutcomeLocked,
		})
		s.renderViewerLogin(w, viewerLoginPageData{
			Username: usernameRaw,
			Locked:   true,
		})
		return
	}

	info, hash, err := s.mockMgr.LookupByUsername(r.Context(), username)
	if err != nil || hash == "" {
		s.viewerLimiter.RegisterFailure(ip, username)
		reason := "user_not_found"
		if err == nil && hash == "" {
			reason = "no_password_set"
		}
		log.Info("denied", "reason", reason, "lookup_err", err)
		s.recordAudit(r, loginaudit.Entry{
			Realm:     loginaudit.RealmViewer,
			Username:  usernameRaw,
			IP:        ip,
			UserAgent: ua,
			Outcome:   loginaudit.OutcomeFail,
		})
		s.renderViewerLogin(w, viewerLoginPageData{
			Username:     usernameRaw,
			ErrorMessage: "Falscher Benutzername oder Passwort.",
		})
		return
	}

	pepper, perr := s.platformCfg.GetSecret(r.Context(), platformconfig.KeyViewerPwPepper)
	if perr != nil {
		log.Warn("pepper lookup failed", "err", perr)
	}
	ok, verr := argon2id.VerifyWithPepper(password, pepper, hash)
	if verr != nil || !ok {
		s.viewerLimiter.RegisterFailure(ip, username)
		log.Info("denied", "reason", "argon2_verify_failed",
			"verify_ok", ok, "verify_err", verr,
			"viewer_mac", info.MAC,
		)
		s.recordAudit(r, loginaudit.Entry{
			Realm:     loginaudit.RealmViewer,
			Username:  usernameRaw,
			ViewerMAC: info.MAC,
			IP:        ip,
			UserAgent: ua,
			Outcome:   loginaudit.OutcomeFail,
		})
		s.renderViewerLogin(w, viewerLoginPageData{
			Username:     usernameRaw,
			ErrorMessage: "Falscher Benutzername oder Passwort.",
		})
		return
	}

	s.viewerLimiter.RegisterSuccess(username)

	// Bestehende Session desselben Browsers wird verworfen, damit
	// es keinen Doppel-Eintrag gibt. Andere Sessions des Viewers
	// (Tablet im Flur, Handy in der Tasche) bleiben.
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
		Username:  usernameRaw,
		ViewerMAC: info.MAC,
		IP:        ip,
		UserAgent: ua,
		Outcome:   loginaudit.OutcomeSuccess,
	})

	// WICHTIG: Set-Cookie MUSS vor http.Redirect rausgehen, sonst
	// schreibt Redirect zuerst WriteHeader und unsere Cookie-Header
	// landen auf der falschen Seite des Status-Codes.
	s.setSessionCookie(w, sid)
	log.Info("granted",
		"viewer_mac", info.MAC,
		"session_prefix", sidPrefix(sid),
		"cookie_name", s.viewerCookieName(),
		"cookie_path", s.viewerCookiePath(),
		"cookie_secure", !s.cfg.DevMode,
	)
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

// normalizeUsername bringt den Benutzernamen auf die Form, in der
// er in der DB liegt: lowercase, Umlaute zu ae/oe/ue/ss aufgeloest,
// Whitespace getrimmt. Mieter sollen "Daemgen", "daemgen" oder
// "Dämgen" eingeben koennen und jedes Mal denselben DB-Eintrag
// finden.
func normalizeUsername(s string) string {
	s = strings.TrimSpace(s)
	s = expandGermanUmlauts(s)
	return strings.ToLower(s)
}

func expandGermanUmlauts(s string) string {
	r := strings.NewReplacer(
		"ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss",
		"Ä", "ae", "Ö", "oe", "Ü", "ue",
	)
	return r.Replace(s)
}

// sidPrefix gibt die ersten 8 Zeichen einer Session-ID zurueck;
// reicht fuer ein Log-Korrelations-Token ohne die volle ID zu
// leaken.
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
