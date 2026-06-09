// Tiny JSON endpoint that feeds the (upcoming, S20-E5) per-viewer
// camera multi-select UI from the live Protect camera list
// (streams.StreamBackend.ListCameras). Saison 20-E1: this MIRRORS
// /a/doors.json - same admin auth, same {configured, <items>}
// envelope, same degrade-to-empty behaviour. Only the JSON endpoint
// lands in E1; the dropdown UI is E5.
package httpserver

import (
	"encoding/json"
	"net/http"
)

// cameraJSON is the per-camera shape the assignment dropdown consumes
// (value_key=id, label_key=name; online + has_package_cam as metadata).
// CONNECTED Protect cameras map to online=true on the backend side.
type cameraJSON struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Online        bool   `json:"online"`
	HasPackageCam bool   `json:"has_package_cam"`
}

// handleAdminCamerasJSON returns the Protect cameras the stream backend
// can reach, in the dropdown shape.
//
// configured=false means no stream backend is wired (public build or no
// Protect access); the UI renders the dropdown empty with the
// placeholder option only - exactly like /a/doors.json when the UA-API
// is unconfigured. The streams field is never nil (main wires
// streams.Unconfigured() when no backend is set), so Configured() is the
// right gate here rather than a nil-check.
func (s *Server) handleAdminCamerasJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if !s.streams.Configured() {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"configured": false,
			"cameras":    []any{},
		})
		return
	}
	cams, err := s.streams.ListCameras(r.Context())
	if err != nil {
		s.log.Warn("cameras.json failed", "err", err)
		http.Error(w, "stream backend error: "+err.Error(), http.StatusBadGateway)
		return
	}
	rows := make([]cameraJSON, 0, len(cams))
	for _, c := range cams {
		rows = append(rows, cameraJSON{
			ID:            c.ID,
			Name:          c.Name,
			Online:        c.Online,
			HasPackageCam: c.HasPackageCam,
		})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"configured": true,
		"cameras":    rows,
	})
}
