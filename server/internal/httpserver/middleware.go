package httpserver

import (
	"context"
	"net/http"
)

// contextKey is unexported so other packages cannot stuff their
// own value under our key.
type contextKey int

const ctxKeyViewerMAC contextKey = 0

// ViewerMACFromContext reads the viewer_mac that requireSession
// stored on the request context. Returns "" if absent (which
// should only happen for handlers that are not behind
// requireSession).
func ViewerMACFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyViewerMAC).(string)
	return v
}

// requireSession is the auth middleware for /webviewer/ routes
// other than the login endpoints. It reads the session cookie,
// validates it (which also performs rolling renewal), and
// stashes the viewer_mac on the request context. Missing or
// invalid session: redirect to /login with 303 See Other so
// browsers downgrade the next request to GET.
//
// The post-login tree was renamed from /einloggen/ to
// /webviewer/; the auth mechanism (bcrypt + session cookie)
// stayed the same.
func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sid := s.readSessionCookie(r)
		if sid == "" {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		viewerMAC, err := s.sessions.Validate(r.Context(), sid)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyViewerMAC, viewerMAC)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
