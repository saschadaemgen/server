package httpserver

import "net/http"

// Admin-Session-Cookie. Wie das Viewer-Cookie: __Host-Prefix und
// Path=/ in Production, "unifix_a_session" mit Path=/a/ in DevMode.
const (
	adminCookieNameSecure = "__Host-unifix_admin"
	adminCookieNameDev    = "unifix_a_session"
	adminCookiePathSecure = "/"
	adminCookiePathDev    = "/a/"
)

// AdminSessionCookieName / -Path sind Test-Defaults.
const AdminSessionCookieName = adminCookieNameDev
const AdminSessionCookiePath = adminCookiePathDev

func (s *Server) adminCookieName() string {
	if s.cfg.DevMode {
		return adminCookieNameDev
	}
	return adminCookieNameSecure
}

func (s *Server) adminCookiePath() string {
	if s.cfg.DevMode {
		return adminCookiePathDev
	}
	return adminCookiePathSecure
}

func (s *Server) setAdminSessionCookie(w http.ResponseWriter, sessionID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.adminCookieName(),
		Value:    sessionID,
		Path:     s.adminCookiePath(),
		HttpOnly: true,
		Secure:   !s.cfg.DevMode,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   sessionCookieMaxAge,
	})
}

func (s *Server) clearAdminSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.adminCookieName(),
		Value:    "",
		Path:     s.adminCookiePath(),
		HttpOnly: true,
		Secure:   !s.cfg.DevMode,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

func (s *Server) readAdminSessionCookie(r *http.Request) string {
	c, err := r.Cookie(s.adminCookieName())
	if err != nil {
		return ""
	}
	return c.Value
}
