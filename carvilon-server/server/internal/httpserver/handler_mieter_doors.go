// Saison 19-30: GET /webviewer/doors - the authenticated viewer's
// assigned doors (viewer_doors, 1:n), names enriched from the live
// UA-API door list. The client renders one unlock button per door
// and unlocks the concrete door_id; this replaces the bare "standby"
// assumption. Auth is requireViewerAuth (Bearer for android/esp,
// cookie for web). Small JSON, so it rides the cloud control relay.
package httpserver

import (
	"encoding/json"
	"net/http"
)

type mieterDoorView struct {
	DoorID string `json:"door_id"` // UA door UUID
	Name   string `json:"name"`    // best display name (label > live name > uuid)
	Label  string `json:"label"`   // admin override, may be empty
	Sort   int    `json:"sort"`
}

func (s *Server) handleMieterDoors(w http.ResponseWriter, r *http.Request) {
	viewerMAC := ViewerMACFromContext(r.Context())
	if viewerMAC == "" {
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}
	assigned, err := s.viewerMgr.ListViewerDoors(r.Context(), viewerMAC)
	if err != nil {
		s.log.Error("mieter doors list", "err", err, "mac_prefix", safePrefix(viewerMAC))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Resolve names from the live door list (one cached UA round-trip
	// per request). Degrades to label/door_id when UA is unreachable.
	meta := s.loadDoorMeta(r.Context())
	nameByID := make(map[string]string, len(meta.allDoors))
	for _, d := range meta.allDoors {
		nameByID[d.ID] = doorShortName(d)
	}

	out := make([]mieterDoorView, 0, len(assigned))
	for _, a := range assigned {
		name := a.Label
		if name == "" {
			name = nameByID[a.DoorID]
		}
		if name == "" {
			name = a.DoorID
		}
		out = append(out, mieterDoorView{
			DoorID: a.DoorID,
			Name:   name,
			Label:  a.Label,
			Sort:   a.Sort,
		})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"doors": out})
}
