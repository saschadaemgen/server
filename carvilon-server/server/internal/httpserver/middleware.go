package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"carvilon.local/server/internal/viewermanager"
)

// contextKey is unexported so other packages cannot stuff their
// own value under our key.
type contextKey int

const ctxKeyViewerMAC contextKey = 0

// ViewerMACFromContext reads the viewer_mac that requireViewerAuth
// stored on the request context. Returns "" if absent (which
// should only happen for handlers that are not behind
// requireViewerAuth).
func ViewerMACFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyViewerMAC).(string)
	return v
}

// requireViewerAuth is the auth middleware for the /webviewer/
// route family. It accepts EITHER:
//
//   - a Bearer token in the Authorization header (Android viewer +
//     any future device that holds a row in viewers with a
//     non-empty device_token_hash), or
//   - a session cookie (the browser web-viewer post-login flow).
//
// Both paths set ctxKeyViewerMAC, so the handlers downstream read
// ViewerMACFromContext without caring how the caller authenticated.
//
// Auth attempt order (and why):
//
//  1. Bearer first when an Authorization: Bearer header is
//     present. Native-app and pure-API clients live here.
//  2. Cookie as fallback. The Saison 15-01 mieter-offer test
//     showed why a Bearer failure must NOT immediately 401: a
//     browser can carry a stale Authorization header from an
//     earlier API-style fetch while still holding a valid
//     session cookie. Falling through preserves the bit-
//     identical browser experience the briefing required.
//
// Final outcome (Bearer try -> Cookie try):
//
//	Bearer ok                            -> 200, viewer_mac set
//	Bearer fail | Cookie ok              -> 200, viewer_mac set
//	Bearer fail | Cookie fail | Bearer header present -> 401
//	Bearer fail | Cookie fail | no Bearer header      -> 303 /login
//	no Bearer  | no Cookie               -> 303 /login
//
// The 401-vs-303 split at the end is the friendly bit: an API
// client (recognisable by the Authorization header) gets a clean
// 401 so the app can re-adopt the token, a browser gets the
// usual login redirect.
//
// Saison 16 Etappe 1 introduced this combined gate to let the
// Android viewer share /webviewer/ routes with the browser. The
// previous Cookie-only requireSession was removed.
func (s *Server) requireViewerAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1) Bearer try. Success short-circuits; failure falls
		//    through so a browser carrying a stale header but a
		//    valid cookie still gets in.
		bearerPresent := false
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			bearerPresent = true
			presented := strings.TrimPrefix(auth, "Bearer ")
			if mac, err := s.viewerMgr.LookupDeviceMACByToken(r.Context(), presented); err == nil {
				ctx := context.WithValue(r.Context(), ctxKeyViewerMAC, mac)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			} else if !errors.Is(err, viewermanager.ErrViewerNotFound) {
				s.log.Error("device bearer lookup", "err", err)
				// Internal error, but treat as auth-fail and
				// continue: maybe the cookie still works.
			}
		}

		// 2) Cookie try.
		sid := s.readSessionCookie(r)
		if sid != "" {
			if viewerMAC, err := s.sessions.Validate(r.Context(), sid); err == nil {
				ctx := context.WithValue(r.Context(), ctxKeyViewerMAC, viewerMAC)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}

		// 3) Both paths failed (or were absent). Refuse in the
		//    style that matches the client: 401 if it tried the
		//    Bearer path, 303 otherwise.
		if bearerPresent {
			http.Error(w, "invalid bearer token", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}
