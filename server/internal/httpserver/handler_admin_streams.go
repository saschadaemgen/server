// Admin read-only view of the stream-server's profile registry.
//
// The page lives under /a/streams and renders against whichever
// backend the operator has wired. With the public-build default
// (Unconfigured), the page renders a "Stream-Backend nicht
// konfiguriert" banner. With the transitional REST client, List
// fetches /api/profiles and the page tabulates the result.
//
// Write surface (PUT/DELETE) intentionally absent from the UI:
// the stream-server is still unifying GET/PUT field-name casing,
// so any local write form against the current format would be
// throwaway work. The streams.StreamBackend interface keeps
// Put/Delete for the eventual follow-up; the admin UI just does
// not call them yet.
//
// Routes (registered in server.go):
//
//	GET    /a/streams                 list-view (HTML)
//	GET    /a/streams.json            list payload as JSON (used
//	                                   by the viewer-edit modal
//	                                   stream-profile dropdown)
package httpserver

import (
	"encoding/json"
	"net/http"
)

// adminStreamsData is the payload for templates/admin/streams.html.
type adminStreamsData struct {
	User       adminUser
	Configured bool   // false = no stream-backend URL set
	BackendURL string // for the "API: <url>" hint line
	Profiles   []streamRow
	Flash      string
	FlashType  string
}

// streamRow is one row in the admin profile list. Fields mirror
// the stream-server's /api/profiles entry shape (the 11-field
// snake_case schema on streams.Profile) so the template can
// render them without further mapping.
type streamRow struct {
	Name          string
	Codec         string
	Usage         string
	Width         int
	Height        int
	FPS           int
	EncodeQuality int
	CameraID      string
	Description   string
	Quality       string
	Encryption    string
}

// streamBackendBaseURL is a soft accessor for the operator-facing
// "API: <url>" hint. The StreamBackend interface intentionally
// has no BaseURL method (the seam exposes URLs only via
// MJPEGURL/WebRTCSignalURL); the admin UI just wants a display
// string for the banner. We type-assert against the concrete
// Client; the future commercial backend can grow the same
// accessor when it lands. Unknown backends return "".
type baseURLer interface{ BaseURL() string }

func (s *Server) handleAdminStreamsList(w http.ResponseWriter, r *http.Request) {
	data := s.buildStreamsData(r)
	s.renderAdminPage(w, "streams", data)
}

// handleAdminStreamsListJSON feeds the viewer-edit modal dropdown.
// Same data as the HTML list but as JSON; the page-render cost is
// modest, but the dropdown polls on open and we keep that path
// header-only.
func (s *Server) handleAdminStreamsListJSON(w http.ResponseWriter, r *http.Request) {
	data := s.buildStreamsData(r)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	out := map[string]any{
		"configured": data.Configured,
		"profiles":   data.Profiles,
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) buildStreamsData(r *http.Request) adminStreamsData {
	username := AdminUserFromContext(r.Context())
	data := adminStreamsData{
		User: adminUser{Name: username, Initials: initialsOf(username)},
	}
	if !s.streams.Configured() {
		return data
	}
	data.Configured = true
	if u, ok := s.streams.(baseURLer); ok {
		data.BackendURL = u.BaseURL()
	}
	profiles, err := s.streams.List(r.Context())
	if err != nil {
		s.log.Warn("admin streams list", "err", err)
		data.Flash = "Stream-Backend nicht erreichbar: " + err.Error()
		data.FlashType = "red"
		return data
	}
	for _, p := range profiles {
		data.Profiles = append(data.Profiles, streamRow{
			Name:          p.Name,
			Codec:         p.Codec,
			Usage:         p.Usage,
			Width:         p.Width,
			Height:        p.Height,
			FPS:           p.FPS,
			EncodeQuality: p.EncodeQuality,
			CameraID:      p.CameraID,
			Description:   p.Description,
			Quality:       p.Quality,
			Encryption:    p.Encryption,
		})
	}
	return data
}
