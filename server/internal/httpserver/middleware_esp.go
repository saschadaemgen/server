package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"carvilon.local/server/internal/viewermanager"
)

type espContextKey int

const ctxKeyESPMAC espContextKey = 0

// ESPMACFromContext returns the ESP-Viewer MAC that the bearer
// auth middleware stashed on the request context. Returns ""
// if absent.
func ESPMACFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyESPMAC).(string)
	return v
}

// requireESPBearer is the auth gate for the protected /esp/-tree.
// It expects a header of the form "Authorization: Bearer <token>",
// hashes the presented token (esptoken.Verify uses
// crypto/subtle.ConstantTimeCompare against every adopted
// ESP-Viewer's stored hash), and exposes the matched MAC on the
// request context.
func (s *Server) requireESPBearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		presented := strings.TrimPrefix(auth, "Bearer ")
		mac, err := s.viewerMgr.LookupESPMACByToken(r.Context(), presented)
		if err != nil {
			if !errors.Is(err, viewermanager.ErrViewerNotFound) {
				s.log.Error("esp bearer lookup", "err", err)
			}
			http.Error(w, "invalid bearer token", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyESPMAC, mac)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
