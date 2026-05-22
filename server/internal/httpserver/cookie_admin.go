package httpserver

import "net/http"

// Admin session cookie. Same scheme as the viewer cookie:
// __Host- prefix and Path=/ in production, "carvilon_a_session"
// with Path=/a/ in DevMode.
const (
	adminCookieNameSecure = "__Host-carvilon_admin"
	adminCookieNameDev    = "carvilon_a_session"
	adminCookiePathSecure = "/"
	adminCookiePathDev    = "/a/"
)

// AdminSessionCookieName / AdminSessionCookiePath are test
// defaults; production code goes through the Server methods.
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
