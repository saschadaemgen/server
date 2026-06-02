// Saison 19: the "stream-start" bundle for a remote (Android) subscriber.
//
// One authenticated call returns everything a remote viewer needs to open a
// WHEP subscription through CGNAT: the public WHEP URL, a short-lived,
// sid-bound egress token (minted edge-local, exactly like
// /webviewer/egress-token), and a fresh set of subscriber ICE servers pulled
// from the cloud over the side-channel (the cloud holds the TURN shared
// secret; the edge never does).
//
// The endpoint lives on the EDGE because only the edge has the viewer auth
// (requireViewerAuth -> the MAC) and the egress signing key. It pulls the
// cloud-held half (ICE) via the request_ice/ice_servers RPC. A cloud outage
// degrades to 503 and never touches the local LAN path (Grundregel).
package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"carvilon.local/server/internal/egresstoken"
)

// streamStartICETimeout bounds the cloud ICE round-trip for one bundle
// request. The edge stays responsive: a slow/dead cloud yields a prompt 503.
const streamStartICETimeout = 5 * time.Second

// handleMieterStreamStart assembles the stream-start bundle for the
// authenticated viewer. Route: GET /webviewer/stream-start (requireViewerAuth).
//
// Order is fail-fast: cheap local config checks first, then the cloud ICE
// round-trip, then the (local) egress mint - so a misconfiguration or a down
// cloud link short-circuits before any token is minted. Failures never leak
// which check failed; the client sees a bare status, the detail goes to the
// log only.
func (s *Server) handleMieterStreamStart(w http.ResponseWriter, r *http.Request) {
	mac := ViewerMACFromContext(r.Context())
	if mac == "" {
		// Behind requireViewerAuth this cannot normally happen; defensive.
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}
	// Egress mint must be configured (same key as /webviewer/egress-token).
	if s.egressIssuer == nil {
		s.log.Warn("stream-start requested but egress token not configured "+
			"(set CARVILON_EGRESS_TOKEN_HMAC_KEY to enable)", "viewer_mac", mac)
		http.Error(w, "stream start not configured", http.StatusServiceUnavailable)
		return
	}
	// WHEP URL: derived (interim) from the edge's cloud WHIP ingress config.
	whepURL, err := deriveWHEPURL(s.cfg.SidechannelCloudWhipURL, mac)
	if err != nil {
		s.log.Warn("stream-start: cannot derive WHEP URL "+
			"(set CARVILON_SIDECHANNEL_CLOUD_WHIP_URL)", "viewer_mac", mac, "err", err)
		http.Error(w, "stream start not configured", http.StatusServiceUnavailable)
		return
	}
	// Subscriber ICE: pulled from the cloud. No client wired (LAN-only edge)
	// or no answer -> 503 "ice unavailable"; the local path is unaffected.
	if s.iceRequester == nil {
		s.log.Warn("stream-start requested but no side-channel client (cloud link down)", "viewer_mac", mac)
		http.Error(w, "ice unavailable", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), streamStartICETimeout)
	defer cancel()
	ice, err := s.iceRequester.RequestICE(ctx)
	if err != nil || len(ice) == 0 {
		s.log.Warn("stream-start: ICE pull failed", "viewer_mac", mac, "err", err, "ice_servers", len(ice))
		http.Error(w, "ice unavailable", http.StatusServiceUnavailable)
		return
	}
	// Local egress mint last, once the bundle is otherwise complete.
	token, err := s.egressIssuer.Issue(mac)
	if err != nil {
		s.log.Error("stream-start: egress token issue failed", "viewer_mac", mac, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Never log the token; only the viewer + counts.
	s.log.Info("stream-start bundle issued", "viewer_mac", mac, "ice_servers", len(ice))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"whep_url":     whepURL,
		"egress_token": token,
		"stream_id":    mac,
		"ice_servers":  ice,
		"expires_in":   int(egresstoken.TTL.Seconds()),
	})
}

// deriveWHEPURL builds the interim WHEP egress URL from the cloud WHIP ingress
// URL: same scheme + host (incl. port), path /whep/<mac>. The cloud serves
// WHIP and WHEP on the same in-process listener, so the host:port carries
// over. Baustufe 1 only - this rides the private cloudca on the VPS IP;
// Baustufe 2 replaces it with a public, browser-trusted WHEP hostname
// delivered from the cloud.
func deriveWHEPURL(cloudWhipURL, mac string) (string, error) {
	if cloudWhipURL == "" {
		return "", errors.New("cloud whip url not set")
	}
	u, err := url.Parse(cloudWhipURL)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("cloud whip url missing scheme/host: %q", cloudWhipURL)
	}
	return fmt.Sprintf("%s://%s/whep/%s", u.Scheme, u.Host, mac), nil
}
