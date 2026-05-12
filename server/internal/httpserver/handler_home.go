package httpserver

import (
	"net/http"
)

type mieterHomeData struct {
	UAUserID string
}

// handleHome renders the tenant landing page. The page hosts an
// EventSource subscription on /m/events and a hidden doorbell
// overlay that the inline JS surfaces when a doorbell_start
// event lands.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	ua := UAUserIDFromContext(r.Context())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.renderMieter(w, "home", mieterHomeData{UAUserID: ua}); err != nil {
		s.log.Error("render mieter home", "err", err)
	}
}
