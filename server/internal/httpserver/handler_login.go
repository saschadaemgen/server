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
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	// Already logged in? Skip token consumption.
	if sid := readSessionCookie(r); sid != "" {
		if _, err := s.sessions.Validate(r.Context(), sid); err == nil {
			http.Redirect(w, r, "/m/", http.StatusSeeOther)
			return
		}
	}

	token := r.URL.Query().Get("t")
	if token == "" {
		s.renderLoginError(w, http.StatusBadRequest,
			"Magic-Link fehlt",
			"Der Link ist unvollstaendig. Bitte fordere einen neuen Link beim Hausverwalter an.")
		return
	}

	uaUserID, err := s.magic.Consume(r.Context(), token)
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

	sid, err := s.sessions.Create(r.Context(), uaUserID, session.Meta{
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
