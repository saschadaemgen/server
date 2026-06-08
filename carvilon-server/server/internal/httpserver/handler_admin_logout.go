package httpserver

import "net/http"

// handleAdminLogout revokes the admin session and clears the
// cookie. Idempotent under requireAdminSession (which already
// returned earlier if there was no session).
func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	if sid := s.readAdminSessionCookie(r); sid != "" {
		_ = s.adminSessions.Revoke(r.Context(), sid)
	}
	s.clearAdminSessionCookie(w)
	http.Redirect(w, r, "/a/login", http.StatusSeeOther)
}
