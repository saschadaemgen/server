package httpserver

import (
	"context"
	"net/http"
)

// contextKey is unexported so other packages cannot stuff their
// own value under our key.
type contextKey int

const ctxKeyUAUserID contextKey = 0

// UAUserIDFromContext reads the ua_user_id that requireSession
// stored on the request context. Returns "" if absent (which
// should only happen for handlers that are not behind
// requireSession).
func UAUserIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyUAUserID).(string)
	return v
}

// requireSession is the auth middleware for /m/ routes other
// than /m/login. It reads the session cookie, validates it
// (which also performs rolling renewal), and stashes the
// ua_user_id on the request context. Missing or invalid session:
// redirect to /m/login with 303 See Other so browsers downgrade
// the next request to GET.
func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sid := readSessionCookie(r)
		if sid == "" {
			http.Redirect(w, r, "/m/login", http.StatusSeeOther)
			return
		}
		uaUserID, err := s.sessions.Validate(r.Context(), sid)
		if err != nil {
			http.Redirect(w, r, "/m/login", http.StatusSeeOther)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyUAUserID, uaUserID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
