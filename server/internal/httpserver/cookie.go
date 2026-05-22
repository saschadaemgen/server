package httpserver

import "net/http"

// Viewer session cookie.
//
// In production (Secure=true) the __Host- prefix is set; per
// RFC 6265bis that rules out domain pinning, path bypass and
// plain-HTTP cookies entirely. __Host- requires Path=/ so the
// cookie no longer sits under /m/ only - admin and viewer
// cookies use different NAMES and therefore do not collide (see
// cookie_admin.go).
//
// In DevMode (CARVILON_DEV_MODE=1; legacy alias UNIFIX_DEV_MODE
// still accepted by config.lookupEnv, plain HTTP) Secure is not
// possible, which makes the __Host- prefix illegal; browsers
// would reject it. We fall back to "carvilon_viewer" without
// the prefix (accepted trade-off, documented).
//
// MaxAge is quasi-permanent (1 year); the DB-side rolling
// renewal in session.Validate enforces the actual 30-day idle
// timeout.
const (
	viewerCookieNameSecure = "__Host-carvilon_viewer"
	viewerCookieNameDev    = "carvilon_viewer"
	// The cookie now lives under / so both /login and any
	// future path (for example /api/...) see it without a
	// path mismatch. Production needs Path=/ anyway for the
	// __Host- prefix.
	viewerCookiePathSecure = "/"
	viewerCookiePathDev    = "/"
	sessionCookieMaxAge    = 365 * 24 * 3600
)

// SessionCookieName is only a default for tests; production
// code goes through Server.viewerCookieName.
const SessionCookieName = viewerCookieNameDev

// SessionCookiePath is only a default for tests; production
// code goes through Server.viewerCookiePath.
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

// clearSessionCookie clears the viewer session cookie (logout path).
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
