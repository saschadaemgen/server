package httpserver

import (
	"context"
	"net/http"
	"strings"
)

// adminUserPrefix is the namespace marker stored as ua_user_id
// in the sessions table for admin sessions. Saison 12 keeps
// admin sessions in the same table as tenant sessions; the
// prefix disambiguates the two without a second table.
const adminUserPrefix = "_admin_"

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
// /a/login. It reads the admin cookie, validates the session,
// confirms the ua_user_id wears the admin prefix, and exposes
// the bare username on the context.
func (s *Server) requireAdminSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sid := readAdminSessionCookie(r)
		if sid == "" {
			http.Redirect(w, r, "/a/login", http.StatusSeeOther)
			return
		}
		uaUserID, err := s.sessions.Validate(r.Context(), sid)
		if err != nil {
			http.Redirect(w, r, "/a/login", http.StatusSeeOther)
			return
		}
		if !strings.HasPrefix(uaUserID, adminUserPrefix) {
			http.Redirect(w, r, "/a/login", http.StatusSeeOther)
			return
		}
		username := strings.TrimPrefix(uaUserID, adminUserPrefix)
		ctx := context.WithValue(r.Context(), ctxKeyAdminUser, username)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
