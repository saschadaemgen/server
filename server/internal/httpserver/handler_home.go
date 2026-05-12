package httpserver

import (
	"errors"
	"net/http"

	"unifix.local/server/internal/mockmanager"
)

type mieterHomeData struct {
	MockMAC  string
	MockName string
}

// handleHome renders the tenant landing page. The page hosts an
// EventSource subscription on /m/events and a hidden doorbell
// overlay that the inline JS surfaces when a doorbell_start
// event lands.
//
// Saison 12-06: the page identifies the tenant by the mock's
// admin-chosen name (the only human label we have for the
// device). The MAC stays in the markup as an HTML data attribute
// so the EventSource can subscribe to the right channel.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	mac := MockMACFromContext(r.Context())
	info, err := s.mockMgr.GetViewerInfo(r.Context(), mac)
	if err != nil {
		if errors.Is(err, mockmanager.ErrViewerNotFound) {
			http.Redirect(w, r, "/m/login", http.StatusSeeOther)
			return
		}
		s.log.Error("get viewer info", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data := mieterHomeData{
		MockMAC:  info.MAC,
		MockName: info.Name,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.renderMieter(w, "home", data); err != nil {
		s.log.Error("render mieter home", "err", err)
	}
}

// safePrefix returns the first 8 chars of a MAC for logging
// without leaking the full address. Falls back to the whole
// string for unexpectedly short input.
func safePrefix(mac string) string {
	if len(mac) < 8 {
		return mac
	}
	return mac[:8]
}
