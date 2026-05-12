package httpserver

import "net/http"

// Session cookie naming. Path is scoped to /m/ so admin and other
// future sub-trees do not see the tenant session cookie.
const (
	SessionCookieName = "unifix_m_session"
	SessionCookiePath = "/m/"

	// Saison 13-02: the mieter UI dropped the logout button.
	// Mieter sessions are now quasi-permanent: the cookie carries
	// a one-year MaxAge so a tenant can close the browser and
	// come back weeks later still logged in. The DB session row
	// still expires after session.DefaultIdleTimeout (30d
	// rolling), and a longer-absent tenant simply hits the
	// magic-link login flow again.
	sessionCookieMaxAge = 365 * 24 * 3600
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

// Saison 13-02-FIX note: the cookie-swap bug reported by Sascha
// turned out to be the optimistic short-circuit in handler_login,
// not multi-path cookie pollution. The handler now consumes the
// token whenever the URL carries one, and the regression is
// covered by TestMagicLinkSessionSwap_SameBrowser. If a future
// build ever needs to wipe a legacy cookie scope (e.g. an older
// version that scoped to "/"), reintroduce a helper here and
// call it BEFORE setSessionCookie, but mind that any tests that
// pick the first matching cookie via response order will then
// have to filter out the wipe entry.

// readSessionCookie returns the cookie value or "" if absent.
func readSessionCookie(r *http.Request) string {
	c, err := r.Cookie(SessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}
