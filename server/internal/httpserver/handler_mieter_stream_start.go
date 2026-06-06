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
// cloud-held half (subscriber ICE + the cloud's public WHEP base) via the
// request_ice/ice_servers RPC, and builds the WHEP URL from that public base
// (or an interim fallback). A cloud outage degrades to 503 and never touches
// the local LAN path (Grundregel).
package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"carvilon.local/server/internal/config"
	"carvilon.local/server/internal/egresstoken"
	"carvilon.local/server/internal/streampublish"
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
	// Subscriber ICE (+ the cloud's public WHEP base) pulled from the cloud.
	// No client wired (LAN-only edge) or no answer -> 503 "ice unavailable";
	// the local path is unaffected.
	if s.iceRequester == nil {
		s.log.Warn("stream-start requested but no side-channel client (cloud link down)", "viewer_mac", mac)
		http.Error(w, "ice unavailable", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), streamStartICETimeout)
	defer cancel()
	res, err := s.iceRequester.RequestICE(ctx)
	if err != nil || len(res.Servers) == 0 {
		s.log.Warn("stream-start: ICE pull failed", "viewer_mac", mac, "err", err, "ice_servers", len(res.Servers))
		http.Error(w, "ice unavailable", http.StatusServiceUnavailable)
		return
	}
	// WHEP URL: prefer the cloud-advertised public base (browser-trusted, from
	// the public WHEP listener); fall back to the interim base derived from the
	// cloud WHIP ingress (private cloudca / VPS IP) when the cloud advertises
	// no public base.
	whepURL := ""
	if res.WHEPBaseURL != "" {
		whepURL = fmt.Sprintf("%s/whep/%s", res.WHEPBaseURL, mac)
	} else if whepURL, err = deriveWHEPURL(s.cfg.SidechannelCloudWhipURL, mac); err != nil {
		s.log.Warn("stream-start: no WHEP URL (cloud sent no public base and "+
			"CARVILON_SIDECHANNEL_CLOUD_WHIP_URL is unset/invalid)", "viewer_mac", mac, "err", err)
		http.Error(w, "stream start not configured", http.StatusServiceUnavailable)
		return
	}
	// Local egress mint last, once the bundle is otherwise complete.
	token, err := s.egressIssuer.Issue(mac)
	if err != nil {
		s.log.Error("stream-start: egress token issue failed", "viewer_mac", mac, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Saison 19-35: additionally advertise the LAN-direct WHEP URL when the
	// edge LAN-WHEP is active and the edge LAN IP is known. Best-effort and
	// ADDITIVE: a viewer-info miss or inactive LAN-WHEP just omits the field,
	// and the app falls back to the cloud whep_url. The edge keys /whep by
	// PROFILE name (not the MAC) - see edgeWHEPURL.
	edgeURL := ""
	if info, ierr := s.viewerMgr.GetViewerInfo(r.Context(), mac); ierr == nil {
		edgeURL, _ = edgeWHEPURL(s.cfg, info.ResolveStreamProfile())
	}

	// Never log the token; only the viewer + counts.
	s.log.Info("stream-start bundle issued", "viewer_mac", mac,
		"ice_servers", len(res.Servers), "public_whep", res.WHEPBaseURL != "",
		"edge_whep", edgeURL != "")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(streampublish.StreamStartBundle{
		WHEPURL:     whepURL,
		EgressToken: token,
		StreamID:    mac,
		ICEServers:  res.Servers,
		ExpiresIn:   int(egresstoken.TTL.Seconds()),
		EdgeWHEPURL: edgeURL,
	})
}

// edgeWHEPURL builds the LAN-direct WHEP URL for a viewer's stream profile,
// or ("", false) when the LAN-WHEP is not active (StreamLANWHEPICEPort == 0)
// or the edge LAN IP is unknown (ServerIPv4 == "") or the profile is empty.
//
// FLAGGE A (Stream-Chat): the edge keys /whep by the PROFILE NAME, not the
// viewer MAC (like /offer ?src=intercom_web), unlike the cloud which keys
// /whep by MAC. The HTTP port comes from StreamAddr (the existing edge stream
// mux, e.g. :8555) - NOT the ICE/UDP port; StreamLANWHEPICEPort only gates
// activation here. (Saison 19-35)
func edgeWHEPURL(cfg config.Config, profile string) (string, bool) {
	if cfg.StreamLANWHEPICEPort == 0 || cfg.ServerIPv4 == "" || profile == "" {
		return "", false
	}
	_, port, err := net.SplitHostPort(cfg.StreamAddr)
	if err != nil || port == "" {
		return "", false
	}
	return fmt.Sprintf("http://%s:%s/whep/%s", cfg.ServerIPv4, port, profile), true
}

// deriveWHEPURL builds the interim WHEP egress URL from the cloud WHIP ingress
// URL: same scheme + host (incl. port), path /whep/<mac>. The cloud serves
// WHIP and WHEP on the same in-process listener, so the host:port carries
// over. This is now the FALLBACK, used only when the cloud advertises no
// public WHEP base (whep_base_url empty): it rides the private cloudca on the
// VPS IP and is NOT browser-trusted. When the cloud sends a public base
// (Baustufe 2), the handler prefers it over this.
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
