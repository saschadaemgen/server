package httpserver

import "net/http"

// handleLogout processes POST /m/logout. requireSession already
// guaranteed a valid session, so we revoke the cookie's value
// and clear it. If the cookie somehow vanished between middleware
// and handler we still clear and redirect (idempotent).
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if sid := readSessionCookie(r); sid != "" {
		_ = s.sessions.Revoke(r.Context(), sid)
	}
	s.clearSessionCookie(w)
	http.Redirect(w, r, "/m/login", http.StatusSeeOther)
}
