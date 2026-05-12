package httpserver

import "net/http"

// Session cookie naming. Path is scoped to /m/ so admin and other
// future sub-trees do not see the tenant session cookie.
const (
	SessionCookieName = "unifix_m_session"
	SessionCookiePath = "/m/"

	sessionCookieMaxAge = 30 * 24 * 3600
)

// setSessionCookie writes the session cookie. Secure is on
// outside DevMode; SameSite is always Strict.
func (s *Server) setSessionCookie(w http.ResponseWriter, sessionID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    sessionID,
		Path:     SessionCookiePath,
		HttpOnly: true,
		Secure:   !s.cfg.DevMode,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   sessionCookieMaxAge,
	})
}

// clearSessionCookie overwrites the cookie with MaxAge=-1, which
// instructs the browser to drop it immediately.
func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     SessionCookiePath,
		HttpOnly: true,
		Secure:   !s.cfg.DevMode,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// readSessionCookie returns the cookie value or "" if absent.
func readSessionCookie(r *http.Request) string {
	c, err := r.Cookie(SessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}
