// Saison 14-01: tenant MJPEG passthrough.
//
// The mieter ringing overlay renders an <img src="/einloggen/
// stream.mjpeg"> while a doorbell call is active. This handler
// resolves the calling tenant's mock-viewer, picks its stream
// profile, and proxies the live MJPEG body from go2rtc with the
// same flush-per-chunk core used by /esp/stream.mjpeg.
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
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}
	info, err := s.mockMgr.GetViewerInfo(r.Context(), mac)
	if err != nil {
		if errors.Is(err, mockmanager.ErrViewerNotFound) {
			http.Error(w, "viewer not found", http.StatusNotFound)
			return
		}
		s.log.Error("mieter stream get viewer", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	profile := info.ResolveStreamProfile()
	s.proxyMJPEGStream(w, r, profile, "mieter", mac)
}
