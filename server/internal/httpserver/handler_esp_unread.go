// Saison 14-XX: GET /esp/unread-count.
//
// Bearer-Auth-gating, identische Response wie
// /webviewer/unread-count: {"count": N}. Beide Endpoints lesen
// aus doorhistory.UnreadCount; die Auth-Mechanik (Cookie vs
// Bearer) ist der einzige Unterschied.
package httpserver

import (
	"encoding/json"
	"net/http"
)

func (s *Server) handleESPUnreadCount(w http.ResponseWriter, r *http.Request) {
	mac := ESPMACFromContext(r.Context())
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
