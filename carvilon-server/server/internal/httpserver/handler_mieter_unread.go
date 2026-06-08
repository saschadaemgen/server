// Read-side endpoint for the screensaver unread-doorbell badge.
//
// Route: GET /webviewer/unread-count (requireViewerAuth)
// Body:  {"count": N}
//
// The badge in intercom-idle.html calls this on page load to
// hydrate its initial number before the first SSE unread-count
// frame arrives. It also re-queries when the live wiring breaks
// (SSE reconnect) so the badge does not stay stuck.
//
// Idempotent, read-only, no side effects - mark-read still goes
// through /webviewer/history.json (which marks the displayed
// rows asynchronously after the JSON ships).
package httpserver

import (
	"encoding/json"
	"net/http"
)

type mieterUnreadResponse struct {
	Count int `json:"count"`
}

func (s *Server) handleMieterUnreadCount(w http.ResponseWriter, r *http.Request) {
	mac := ViewerMACFromContext(r.Context())
	if mac == "" {
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}
	count := 0
	if s.history != nil {
		n, err := s.history.UnreadCount(r.Context(), mac)
		if err != nil {
			s.log.Warn("doorhistory unread count failed",
				"mac_prefix", safePrefix(mac), "err", err)
			http.Error(w, "unread count failed", http.StatusInternalServerError)
			return
		}
		count = n
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(mieterUnreadResponse{Count: count})
}
