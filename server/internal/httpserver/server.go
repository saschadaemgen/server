// Package httpserver hosts the tenant-facing HTTP layer. It
// wires three endpoints under /m/: login (public, consumes a
// magic-link token), home (session-protected stub), and logout
// (session-protected, revokes and clears the cookie).
//
// Pure net/http with Go 1.22 ServeMux pattern routing. No router
// or web-framework dependency. TLS is provided by the standard
// library; in DevMode the listener is plain HTTP and the Secure
// cookie flag is disabled.
package httpserver

import (
	"net/http"

	"unifix.local/server/internal/auth/magiclink"
	"unifix.local/server/internal/auth/session"
	"unifix.local/server/internal/config"
)

// Server owns the mux and references the auth services.
type Server struct {
	cfg      config.Config
	magic    *magiclink.Service
	sessions *session.Service
	mux      *http.ServeMux
}

// New constructs the Server with all routes registered.
func New(cfg config.Config, m *magiclink.Service, s *session.Service) *Server {
	srv := &Server{
		cfg:      cfg,
		magic:    m,
		sessions: s,
		mux:      http.NewServeMux(),
	}
	srv.routes()
	return srv
}

func (s *Server) routes() {
	// /m/login is public; everything else under /m/ requires a
	// valid session. The more specific GET /m/login pattern wins
	// over the GET /m/ prefix per Go 1.22 ServeMux precedence.
	s.mux.HandleFunc("GET /m/login", s.handleLogin)
	s.mux.Handle("POST /m/logout", s.requireSession(http.HandlerFunc(s.handleLogout)))
	s.mux.Handle("GET /m/", s.requireSession(http.HandlerFunc(s.handleHome)))
}

// Handler returns the underlying mux so callers (tests) can wrap
// it in an httptest.Server.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// ListenAndServe blocks. In DevMode it serves plain HTTP, in TLS
// mode it serves https from CertFile and KeyFile.
func (s *Server) ListenAndServe() error {
	if s.cfg.DevMode {
		return http.ListenAndServe(s.cfg.ListenAddr, s.mux)
	}
	return http.ListenAndServeTLS(s.cfg.ListenAddr, s.cfg.CertFile, s.cfg.KeyFile, s.mux)
}
