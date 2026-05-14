// Saison 13-07: tiny JSON endpoint that lets the viewer edit /
// adopt modals populate their "Verknuepfte Klingel"-Dropdown
// without having to ship the full intercom-mapping page. Returns
// the live UA-API intercom list filtered by uaapi.ListIntercoms.
package httpserver

import (
	"encoding/json"
	"net/http"
)

type intercomJSON struct {
	ID         string `json:"id"`
	MAC        string `json:"mac"`
	Name       string `json:"name"`
	DeviceType string `json:"device_type"`
	Online     bool   `json:"online"`
}

// handleAdminIntercomsJSON returns every intercom-family device
// the UA-API knows about, in the shape the custom-dropdown
// component consumes (root_key=intercoms, label_key=Name,
// value_key=MAC, extra_key=DeviceType).
//
// configured=false means the operator has not entered UA-API
// credentials yet; the modal then renders the dropdown empty
// with the placeholder option only.
func (s *Server) handleAdminIntercomsJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if s.ua == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"configured": false,
			"intercoms":  []any{},
		})
		return
	}
	devices, err := s.ua.ListIntercoms(r.Context())
	if err != nil {
		s.log.Warn("intercoms.json failed", "err", err)
		http.Error(w, "ua api error: "+err.Error(), http.StatusBadGateway)
		return
	}
	rows := make([]intercomJSON, 0, len(devices))
	for _, d := range devices {
		rows = append(rows, intercomJSON{
			ID:         d.ID,
			MAC:        d.DisplayMAC(),
			Name:       d.DisplayName(),
			DeviceType: d.Type,
			Online:     d.IsOnline,
		})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"configured": true,
		"intercoms":  rows,
	})
}
