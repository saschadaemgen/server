// GET /esp/unread-count.
//
// Bearer-auth gated reuse with the same response shape as
// /webviewer/unread-count ({"count": N}). Both endpoints read
// from doorhistory.UnreadCount; the auth mechanism (cookie vs
// bearer) is the only difference.
package httpserver

import (
	"encoding/json"
	"net/http"
)

func (s *Server) handleESPUnreadCount(w http.ResponseWriter, r *http.Request) {
	mac := DeviceMACFromContext(r.Context())
	if mac == "" {
		http.Error(w, "no esp identity", http.StatusUnauthorized)
		return
	}
	count := 0
	if s.history != nil {
		n, err := s.history.UnreadCount(r.Context(), mac)
		if err != nil {
			s.log.Warn("esp unread count failed",
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
