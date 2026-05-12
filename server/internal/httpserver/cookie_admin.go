package httpserver

import "net/http"

// Admin session cookie. Scoped to /a/ so it never gets sent to
// the tenant /m/ tree and vice versa.
const (
	AdminSessionCookieName = "unifix_a_session"
	AdminSessionCookiePath = "/a/"
)

func (s *Server) setAdminSessionCookie(w http.ResponseWriter, sessionID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     AdminSessionCookieName,
		Value:    sessionID,
		Path:     AdminSessionCookiePath,
		HttpOnly: true,
		Secure:   !s.cfg.DevMode,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   sessionCookieMaxAge,
	})
}

func (s *Server) clearAdminSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     AdminSessionCookieName,
		Value:    "",
		Path:     AdminSessionCookiePath,
		HttpOnly: true,
		Secure:   !s.cfg.DevMode,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

func readAdminSessionCookie(r *http.Request) string {
	c, err := r.Cookie(AdminSessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}
