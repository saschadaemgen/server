// Saison 14-01: tenant MJPEG passthrough.
// Saison 14-01-FIX01: structured per-request logging, consistent
// with handler_esp_stream.go.
//
// The mieter ringing overlay renders an <img src="/webviewer/
// stream.mjpeg"> while a doorbell call is active, plus the
// idle-livestream mode (saison-14-01b) keeps it open whenever
// the screensaver is toggled off. This handler resolves the
// calling tenant's mock-viewer, picks its stream profile, and
// proxies the live MJPEG body from go2rtc with the same
// flush-per-chunk + url.Parse core used by /esp/stream.mjpeg.
//
// Auth is the regular session cookie middleware (requireSession);
// no admin path, no API token. Browsers that drop credentials on
// <img> requests would 401 here - we rely on same-origin cookies
// which Chrome / Firefox / Safari all send on same-origin img.
package httpserver

import (
	"errors"
	"net/http"

	"unifix.local/server/internal/mockmanager"
)

func (s *Server) handleMieterStream(w http.ResponseWriter, r *http.Request) {
	mac := ViewerMACFromContext(r.Context())
	if mac == "" {
		s.log.Warn("stream proxy: unauthorized",
			"route", r.URL.Path, "reason", "no session")
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}
	info, err := s.mockMgr.GetViewerInfo(r.Context(), mac)
	if err != nil {
		if errors.Is(err, mockmanager.ErrViewerNotFound) {
			s.log.Warn("stream proxy: viewer not found",
				"route", r.URL.Path, "viewer_mac", mac)
			http.Error(w, "viewer not found", http.StatusNotFound)
			return
		}
		s.log.Error("stream proxy: get viewer failed",
			"route", r.URL.Path, "viewer_mac", mac, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	profile := info.ResolveStreamProfile()
	s.proxyMJPEGStream(w, r, profile, "mieter", mac)
}
