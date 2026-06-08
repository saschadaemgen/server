// Saison 18-14: short-lived WHEP egress-token issuance for the mieter
// browser / native app.
//
// The authenticated tenant (viewer_mac from requireViewerAuth, which is
// the streamID) requests a 5-minute, streamID-bound token here and then
// presents it to the cloud WHEP egress on the VPS. carvilon (the edge)
// is the only authority that knows the tenant maps to this streamID, so
// it ISSUES; the stream-cloud only VERIFIES (its own publishtoken.Verify
// under the egress key). The token byte-format is identical to a publish
// token; the separate egress key is the domain separation.
package httpserver

import (
	"encoding/json"
	"net/http"

	"carvilon.local/server/internal/egresstoken"
)

// handleMieterEgressToken mints a short-lived egress token for the
// authenticated viewer. Route: GET /webviewer/egress-token
// (requireViewerAuth).
//
// Soft-gated: when no egress key is configured the issuer is nil and the
// endpoint returns 503 (the cloud egress is additive, never a crash).
// Failures never leak which check failed - the client sees a bare
// status, the detail goes only to the log.
func (s *Server) handleMieterEgressToken(w http.ResponseWriter, r *http.Request) {
	mac := ViewerMACFromContext(r.Context())
	if mac == "" {
		// Behind requireViewerAuth this cannot normally happen; defensive.
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}
	if s.egressIssuer == nil {
		s.log.Warn("egress token requested but not configured "+
			"(set CARVILON_EGRESS_TOKEN_HMAC_KEY to enable)", "viewer_mac", mac)
		http.Error(w, "egress token not configured", http.StatusServiceUnavailable)
		return
	}
	token, err := s.egressIssuer.Issue(mac)
	if err != nil {
		s.log.Error("egress token issue failed", "viewer_mac", mac, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Never log the token itself, only the viewer it was minted for.
	s.log.Info("egress token issued", "viewer_mac", mac)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token":      token,
		"stream_id":  mac,
		"expires_in": int(egresstoken.TTL.Seconds()),
	})
}
