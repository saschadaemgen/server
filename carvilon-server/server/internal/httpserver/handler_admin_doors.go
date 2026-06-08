// Tiny JSON endpoint that feeds the per-viewer door-assignment UI
// from the live UA-API door list (GET /api/v1/developer/doors via
// uaapi.ListDoors). Saison 19-30: this REPLACES /a/intercoms.json as
// the source of the assignment dropdown - we assign the DOOR (the
// thing the DoorController actually opens), not the bell. ListDoors
// is the same call the unlock auto-resolution already relies on, so
// the list is known to populate (unlike the intercom type-filter).
package httpserver

import (
	"encoding/json"
	"net/http"
)

type doorJSON struct {
	DoorID string `json:"door_id"`
	Name   string `json:"name"`
}

// handleAdminDoorsJSON returns every door the UA-API knows about in
// the shape the assignment dropdown consumes (value_key=door_id
// (UUID), label_key=name).
//
// configured=false means the operator has not entered UA-API
// credentials yet; the UI renders the dropdown empty with the
// placeholder option only.
func (s *Server) handleAdminDoorsJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if s.ua == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"configured": false,
			"doors":      []any{},
		})
		return
	}
	doors, err := s.ua.ListDoors(r.Context())
	if err != nil {
		s.log.Warn("doors.json failed", "err", err)
		http.Error(w, "ua api error: "+err.Error(), http.StatusBadGateway)
		return
	}
	rows := make([]doorJSON, 0, len(doors))
	for _, d := range doors {
		rows = append(rows, doorJSON{
			DoorID: d.ID,
			Name:   d.DisplayName(),
		})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"configured": true,
		"doors":      rows,
	})
}
