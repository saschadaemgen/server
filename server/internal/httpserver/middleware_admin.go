package httpserver

import (
	"context"
	"net/http"
)

type adminContextKey int

const ctxKeyAdminUser adminContextKey = 0

// AdminUserFromContext returns the admin username that
// requireAdminSession stashed on the request context. Returns
// "" if absent.
func AdminUserFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyAdminUser).(string)
	return v
}

// requireAdminSession is the auth gate for /a/ pages other than
// /a/login. It reads the admin cookie, validates the session
// against the admin_sessions service, and exposes the admin
// username on the context.
//
// Saison 12-06: admin sessions live in their own table now, no
// more "_admin_<user>" prefix surrogate.
func (s *Server) requireAdminSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sid := readAdminSessionCookie(r)
		if sid == "" {
			http.Redirect(w, r, "/a/login", http.StatusSeeOther)
			return
		}
		username, err := s.adminSessions.Validate(r.Context(), sid)
		if err != nil {
			http.Redirect(w, r, "/a/login", http.StatusSeeOther)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyAdminUser, username)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
