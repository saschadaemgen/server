// Saison 15-01: WebRTC signalling proxy for the mieter web-viewer.
//
// Browsers cannot reach the streaming backend directly (it sits
// on localhost:8555 on the same RPi). The web-viewer JS POSTs an
// SDP offer to /webviewer/offer; this handler forwards the body
// to the backend's signalling endpoint (built via
// WebRTCSignalURL(profile)) and copies the SDP answer back.
//
// Unlike the MJPEG proxy this is a single request/response - no
// hijack, no chunked-multipart fiddling. The body limit is small
// (an SDP offer is a few kilobytes) so we just stream it through.
//
// Auth is the regular session cookie middleware (requireViewerAuth);
// the Authorization header is stripped before forwarding so the
// backend never sees the tenant's bearer token / cookie.
package httpserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"carvilon.local/server/internal/viewermanager"
)

// mieterOfferTimeout caps how long the proxy waits for the
// backend to answer. WebRTC signalling completes in well under a
// second in practice; 10s is generous enough for a re-handshake
// after a transient backend hiccup without keeping a stuck call
// open forever.
const mieterOfferTimeout = 10 * time.Second

// mieterOfferMaxBytes caps the SDP body the client may POST. An
// offer with a single video + audio track plus reasonable ICE
// candidates fits in a few KB; 64 KB leaves head-room for extreme
// browsers without exposing the backend to abuse.
const mieterOfferMaxBytes = 64 * 1024

// handleMieterOffer proxies a WebRTC SDP-offer POST to the
// streaming backend's signalling endpoint and copies the answer
// back to the browser.
//
// Route: POST /webviewer/offer  (requireViewerAuth)
func (s *Server) handleMieterOffer(w http.ResponseWriter, r *http.Request) {
	mac := ViewerMACFromContext(r.Context())
	if mac == "" {
		s.log.Warn("offer proxy: unauthorized",
			"route", r.URL.Path, "reason", "no session")
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}
	if !s.streams.Configured() {
		s.log.Warn("offer proxy: stream backend not configured",
			"route", r.URL.Path, "viewer_mac", mac)
		http.Error(w, "stream backend not configured", http.StatusServiceUnavailable)
		return
	}

	info, err := s.viewerMgr.GetViewerInfo(r.Context(), mac)
	if err != nil {
		if errors.Is(err, viewermanager.ErrViewerNotFound) {
			s.log.Warn("offer proxy: viewer not found",
				"route", r.URL.Path, "viewer_mac", mac)
			http.Error(w, "viewer not found", http.StatusNotFound)
			return
		}
		s.log.Error("offer proxy: get viewer failed",
			"route", r.URL.Path, "viewer_mac", mac, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	profile := info.ResolveStreamProfile()
	backend := s.streams.WebRTCSignalURL(profile)
	if backend == "" {
		s.log.Warn("offer proxy: backend produced empty URL",
			"route", r.URL.Path, "viewer_mac", mac, "profile", profile)
		http.Error(w, "stream backend not configured", http.StatusServiceUnavailable)
		return
	}

	// Read the SDP offer with a hard cap. http.MaxBytesReader
	// makes the read fail (not silently truncate) if the client
	// goes over the limit, which is exactly the behavior we
	// want here: the browser is buggy/malicious if it sends
	// more than 64 KB of SDP.
	offer, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mieterOfferMaxBytes))
	if err != nil {
		s.log.Warn("offer proxy: read body",
			"route", r.URL.Path, "viewer_mac", mac, "err", err)
		http.Error(w, "offer body too large or unreadable", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), mieterOfferTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, backend, bytes.NewReader(offer))
	if err != nil {
		s.log.Error("offer proxy: build request",
			"route", r.URL.Path, "viewer_mac", mac, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Forward the SDP content-type but never the tenant's
	// Authorization header - the backend has its own trust
	// boundary, and leaking session bearers across it would be
	// a privilege-escalation gift.
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	} else {
		req.Header.Set("Content-Type", "application/sdp")
	}
	req.Header.Set("Accept", "application/sdp")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.log.Warn("offer proxy: backend call failed",
			"route", r.URL.Path, "viewer_mac", mac, "profile", profile,
			"backend", backend, "err", err)
		http.Error(w, "stream backend unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy answer back. Status + Content-Type are the only
	// headers the browser cares about; we deliberately drop
	// hop-by-hop and tenant-irrelevant headers.
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/sdp")
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		// Best effort: by now we've already written headers, so
		// we just log.
		s.log.Warn("offer proxy: copy answer",
			"route", r.URL.Path, "viewer_mac", mac, "err", err)
		return
	}
	s.log.Info("offer proxy: forwarded",
		"viewer_mac", mac, "profile", profile,
		"backend_status", resp.StatusCode)
}
