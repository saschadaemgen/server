// Saison 13-08 Phase A: tiny reverse-proxy that fronts whatever
// MJPEG (or HLS / WebRTC if a later season swaps the backend)
// source the operator has plumbed in. Currently a placeholder -
// the real go2rtc / Protect integration lands in S13b. Endpoint
// returns 503 when no backend is configured so the ESP-Chat can
// build the firmware path without a live stream upstream.
package httpserver

import (
	"net/http"
	"net/http/httputil"
	"net/url"
)

// handleESPStream forwards the request to cfg.StreamBackendURL.
// The Bearer-Auth has already happened in requireESPBearer; we
// strip the Authorization header before forwarding so the
// backend (which is typically an unauthenticated localhost
// daemon) does not reject the request and so the ESP's bearer
// token never leaks beyond this process.
//
// On startup-time misconfiguration (URL parse error) the handler
// emits 500; the operator should see the parse error in the
// server log when they save an invalid URL.
func (s *Server) handleESPStream(w http.ResponseWriter, r *http.Request) {
	if s.cfg.StreamBackendURL == "" {
		http.Error(w, "stream backend not configured", http.StatusServiceUnavailable)
		return
	}
	backend, err := url.Parse(s.cfg.StreamBackendURL)
	if err != nil {
		s.log.Error("esp stream backend url invalid", "err", err)
		http.Error(w, "stream backend url invalid", http.StatusInternalServerError)
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(backend)
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = backend.Scheme
		req.URL.Host = backend.Host
		req.URL.Path = backend.Path
		req.URL.RawQuery = backend.RawQuery
		req.Host = backend.Host
		req.Header.Del("Authorization")
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, perr error) {
		s.log.Warn("esp stream backend unreachable", "err", perr)
		http.Error(w, "stream backend unreachable", http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r)
}
