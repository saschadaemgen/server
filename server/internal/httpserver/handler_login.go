package httpserver

import (
	"errors"
	"html/template"
	"net"
	"net/http"

	"unifix.local/server/internal/auth/magiclink"
	"unifix.local/server/internal/auth/session"
)

// loginErrorTpl renders a tiny localized error page. html/template
// auto-escapes Heading and Message, so any future user-derived
// values are XSS-safe.
var loginErrorTpl = template.Must(template.New("login_error").Parse(`<!doctype html>
<html lang="de">
<head><meta charset="utf-8"><title>Login fehlgeschlagen</title></head>
<body>
<h1>{{.Heading}}</h1>
<p>{{.Message}}</p>
</body>
</html>
`))

// handleLogin processes GET /m/login?t=<token>.
//
// Saison 13-02-FIX: the optimistic "already-logged-in" skip used
// to fire on every request that carried a valid session cookie,
// including those that arrived with a fresh token in the URL. The
// result was that clicking a second magic-link in the same browser
// silently kept the first session alive and the second token went
// unconsumed. We now skip token-consume only when the URL carries
// no token at all (the "browser back to /m/login" case the
// optimistic check was actually written for). When a token IS
// present, we consume it, revoke any pre-existing session for
// hygiene, and overwrite the cookie with the fresh session id.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("t")

	// No token: only the "already signed in, hit /m/login by
	// accident" case. Forward to the home page so the magic-link
	// is not burned on a stray reload.
	if token == "" {
		if sid := readSessionCookie(r); sid != "" {
			if _, err := s.sessions.Validate(r.Context(), sid); err == nil {
				http.Redirect(w, r, "/m/", http.StatusSeeOther)
				return
			}
		}
		s.renderLoginError(w, http.StatusBadRequest,
			"Magic-Link fehlt",
			"Der Link ist unvollstaendig. Bitte fordere einen neuen Link beim Hausverwalter an.")
		return
	}

	mockMAC, err := s.magic.Consume(r.Context(), token)
	switch {
	case errors.Is(err, magiclink.ErrTokenNotFound):
		s.renderLoginError(w, http.StatusBadRequest,
			"Link ungueltig",
			"Bitte fordere einen neuen Link beim Hausverwalter an.")
		return
	case errors.Is(err, magiclink.ErrTokenExpired):
		s.renderLoginError(w, http.StatusBadRequest,
			"Link abgelaufen",
			"Der Link ist abgelaufen. Bitte fordere einen neuen Link beim Hausverwalter an.")
		return
	case errors.Is(err, magiclink.ErrTokenConsumed):
		s.renderLoginError(w, http.StatusBadRequest,
			"Link wurde schon benutzt",
			"Der Link kann nur einmal verwendet werden. Bitte fordere einen neuen Link beim Hausverwalter an.")
		return
	case err != nil:
		http.Error(w, "Login fehlgeschlagen", http.StatusInternalServerError)
		return
	}

	// Revoke any pre-existing session before we issue the new one.
	// The cookie overwrite below would already point the browser
	// at the new session, but cleaning up the stale row keeps the
	// DB tidy and prevents the old session being re-used if it
	// somehow shows up again (cookie sync from another tab, etc).
	if oldSID := readSessionCookie(r); oldSID != "" {
		_ = s.sessions.Revoke(r.Context(), oldSID)
	}

	sid, err := s.sessions.Create(r.Context(), mockMAC, session.Meta{
		UserAgent: r.UserAgent(),
		IP:        clientIP(r),
	})
	if err != nil {
		http.Error(w, "Login fehlgeschlagen", http.StatusInternalServerError)
		return
	}

	s.setSessionCookie(w, sid)
	http.Redirect(w, r, "/m/", http.StatusSeeOther)
}

func (s *Server) renderLoginError(w http.ResponseWriter, status int, heading, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = loginErrorTpl.Execute(w, struct {
		Heading string
		Message string
	}{heading, message})
}

// clientIP strips the port from r.RemoteAddr. Falls back to the
// raw value if SplitHostPort fails (e.g. when a test injects a
// bare address).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
