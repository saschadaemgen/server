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

// viewerLoginPageData ist die Payload fuer das Login-Form.
// Saison 13-02-FIX4-a-HOTFIX1: Pre-Fill via URL-Parameter ist
// raus (Sicherheits-Anti-Pattern; Passwoerter sollen nicht in
// Server-Logs / Browser-History / Referer landen).
type viewerLoginPageData struct {
	Username     string
	ErrorMessage string
	Locked       bool
}

// handleLoginGet beantwortet GET /login (Saison 14-02; ersetzt
// handleViewerRoot auf /einloggen).
//
// Mit gueltiger Session: 303 Redirect nach /webviewer/ - die URL-
// Anzeige im Browser bleibt damit sauber (kein /login mit
// gerendertem Home-Body).
// Ohne Session: Login-Form anzeigen.
//
// Saison 13-02-FIX4-a-HOTFIX1: ?u= und ?p= URL-Parameter werden
// IGNORIERT (Pre-Fill via QR-Code wurde entfernt).
func (s *Server) handleLoginGet(w http.ResponseWriter, r *http.Request) {
	if sid := s.readSessionCookie(r); sid != "" {
		if _, err := s.sessions.Validate(r.Context(), sid); err == nil {
			http.Redirect(w, r, "/webviewer/", http.StatusSeeOther)
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
	// Form-Field heisst weiter "username" (Browser-autofill nutzt
	// das HTML-Attribut), aber inhaltlich ist es jetzt der
	// Wohnungs-Name. Audit-Log und Limiter-Bucket nutzen den
	// normalisierten Wert als stabilen Key.
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
		Username:  nameRaw,
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
	http.Redirect(w, r, "/webviewer/", http.StatusSeeOther)
}

// handleViewerLogout revokiert die Viewer-Session und loescht das
// Cookie.
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
