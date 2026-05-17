package httpserver

import "net/http"

// Viewer-Session-Cookie. Saison 13-02-FIX4-a:
//
// In Production (Secure=true) wird der __Host-Prefix gesetzt; das
// schliesst per RFC 6265bis Domain-Pinning, Path-Bypass und HTTP-
// Cookies vollstaendig aus. Damit aendert sich auch der Path: __Host-
// erfordert Path=/ , also liegt das Cookie nicht mehr nur unter
// /m/ . Da Admin- und Viewer-Cookie unterschiedliche Namen haben
// stoert das nicht (siehe cookie_admin.go).
//
// In DevMode (UNIFIX_DEV_MODE=1, plain HTTP) ist Secure unmoeglich
// und damit der __Host-Prefix nicht regelkonform; die Browser
// wuerden ihn ablehnen. Wir fallen dann auf "carvilon_viewer" ohne
// Prefix zurueck (akzeptierter Trade-Off, dokumentiert).
//
// MaxAge ist quasi-permanent (1 Jahr); das DB-rolling-renewal in
// session.Validate sorgt fuer "echte" 30-Tage-Idle-Loesung.
const (
	viewerCookieNameSecure = "__Host-carvilon_viewer"
	viewerCookieNameDev    = "carvilon_viewer"
	// Saison 13-02-FIX4-a-HOTFIX2: Cookie laeuft jetzt unter / ,
	// damit sowohl /einloggen als auch zukuenftige Pfade (z.B.
	// /api/...) das Cookie sehen ohne Path-Mismatch. Production
	// braucht Path=/ ohnehin fuer den __Host-Prefix.
	viewerCookiePathSecure = "/"
	viewerCookiePathDev    = "/"
	sessionCookieMaxAge    = 365 * 24 * 3600
)

// SessionCookieName ist nur ein Default fuer Tests; Production-
// Code geht ueber Server.viewerCookieName.
const SessionCookieName = viewerCookieNameDev

// SessionCookiePath ist nur ein Default fuer Tests; Production-
// Code geht ueber Server.viewerCookiePath.
const SessionCookiePath = viewerCookiePathDev

func (s *Server) viewerCookieName() string {
	if s.cfg.DevMode {
		return viewerCookieNameDev
	}
	return viewerCookieNameSecure
}

func (s *Server) viewerCookiePath() string {
	if s.cfg.DevMode {
		return viewerCookiePathDev
	}
	return viewerCookiePathSecure
}

// setSessionCookie writes the session cookie. Secure is on outside
// DevMode; SameSite is always Strict.
func (s *Server) setSessionCookie(w http.ResponseWriter, sessionID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.viewerCookieName(),
		Value:    sessionID,
		Path:     s.viewerCookiePath(),
		HttpOnly: true,
		Secure:   !s.cfg.DevMode,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   sessionCookieMaxAge,
	})
}

// clearSessionCookie loescht das Viewer-Session-Cookie (Logout-Pfad).
func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.viewerCookieName(),
		Value:    "",
		Path:     s.viewerCookiePath(),
		HttpOnly: true,
		Secure:   !s.cfg.DevMode,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// readSessionCookie returns the cookie value or "" if absent.
func (s *Server) readSessionCookie(r *http.Request) string {
	c, err := r.Cookie(s.viewerCookieName())
	if err != nil {
		return ""
	}
	return c.Value
}
