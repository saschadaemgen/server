// Saison 14-01: admin CRUD for go2rtc stream profiles.
//
// The page lives under /a/streams and proxies the go2rtc REST API
// through the regular admin-session middleware. Calls into
// internal/streams; the actual YAML editing happens server-side
// inside go2rtc, which means changes take effect live (existing
// consumers reconnect on the next chunk; new viewers immediately
// hit the new pipeline).
//
// Routes (registered in server.go):
//   GET    /a/streams                 list-view (HTML)
//   GET    /a/streams.json            list payload as JSON (used
//                                      by the viewer-edit modal
//                                      stream-profile dropdown)
//   POST   /a/streams                 create (form: name, source)
//   GET    /a/streams/{name}          edit-view (HTML, single)
//   POST   /a/streams/{name}          update (form: source)
//   POST   /a/streams/{name}/delete   delete (form)
//   DELETE /a/streams/{name}          delete (JSON / REST flavour)
package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"unifix.local/server/internal/streams"
)

// streamProfileNameRE locks profile names down to a safe alphabet.
// go2rtc itself accepts looser names but our admin UI passes them
// through URL paths and dropdowns, so we keep it boring.
var streamProfileNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// adminStreamsData is the payload for templates/admin/streams.html.
type adminStreamsData struct {
	User        adminUser
	Configured  bool          // false = no go2rtc backend URL set
	BackendURL  string        // for the "go2rtc API:" hint line
	Profiles    []streamRow
	Flash       string
	FlashType   string
	Defaults    []streamDefault // template buttons in the create-modal
}

type streamRow struct {
	Name      string
	Source    string
	Consumers int
}

// adminStreamEditData carries one profile for the edit-view page.
type adminStreamEditData struct {
	User       adminUser
	Configured bool
	BackendURL string
	Profile    streamRow
	Flash      string
	FlashType  string
}

// streamDefault is one preset the admin can click to pre-fill the
// create-form with a known-good ffmpeg pipeline. Saison 14-01
// ships three (intercom_esp, intercom_browser, intercom_high) but
// the operator can add their own profiles freely afterwards.
type streamDefault struct {
	Name        string
	Label       string
	Source      string
	Description string
}

func adminStreamDefaults() []streamDefault {
	return []streamDefault{
		{
			Name:        "intercom_esp",
			Label:       "ESP-Default (800x1280, 9 FPS, q:v 6)",
			Source:      "ffmpeg:intercom_high#video=mjpeg#width=800#height=1280#raw=-r 9 -q:v 6",
			Description: "Liefert das richtige Format fuer die ESP32-P4-Boards. Sweet-Spot aus ESP-Saison 2.",
		},
		{
			Name:        "intercom_browser",
			Label:       "Browser-Default (640x1024, 12 FPS, q:v 5)",
			Source:      "ffmpeg:intercom_high#video=mjpeg#width=640#height=1024#raw=-r 12 -q:v 5",
			Description: "Etwas kleinere Aufloesung fuer Mieter-Browser; ruckelfreier auf alten Mobilgeraeten.",
		},
		{
			Name:        "intercom_high",
			Label:       "Hochaufloesend (Source-Profil)",
			Source:      "rtsps://<udm-ip>:7441/<token>",
			Description: "Source-Profil von dem ESP- und Browser-Profile abgeleitet werden. Setze hier den RTSPS-Pfad aus go2rtc.",
		},
	}
}

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

func (s *Server) handleAdminStreamsCreate(w http.ResponseWriter, r *http.Request) {
	if s.streams == nil {
		http.Error(w, "stream backend not configured", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("name"))
	source := strings.TrimSpace(r.PostForm.Get("source"))
	if !streamProfileNameRE.MatchString(name) {
		http.Error(w, "Profil-Name muss aus Buchstaben, Zahlen, '_', '-' oder '.' bestehen (1-64 Zeichen).", http.StatusBadRequest)
		return
	}
	if source == "" {
		http.Error(w, "Source-URL darf nicht leer sein.", http.StatusBadRequest)
		return
	}
	if err := s.streams.Put(r.Context(), name, []string{source}); err != nil {
		s.log.Error("admin streams create", "err", err, "name", name)
		http.Error(w, "Anlegen fehlgeschlagen: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.log.Info("admin stream profile created", "name", name)
	http.Redirect(w, r, "/a/streams", http.StatusSeeOther)
}

func (s *Server) handleAdminStreamsEdit(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !streamProfileNameRE.MatchString(name) {
		http.Error(w, "ungueltiger Profil-Name", http.StatusBadRequest)
		return
	}
	username := AdminUserFromContext(r.Context())
	data := adminStreamEditData{
		User: adminUser{Name: username, Initials: initialsOf(username)},
	}
	if s.streams == nil {
		data.Configured = false
		data.Profile = streamRow{Name: name}
		s.renderAdminPage(w, "stream-edit", data)
		return
	}
	data.Configured = true
	data.BackendURL = s.streams.BaseURL()
	prof, err := s.streams.Get(r.Context(), name)
	if err != nil {
		if errors.Is(err, streams.ErrProfileNotFound) {
			http.NotFound(w, r)
			return
		}
		s.log.Error("admin streams edit fetch", "err", err, "name", name)
		http.Error(w, "Laden fehlgeschlagen: "+err.Error(), http.StatusBadGateway)
		return
	}
	data.Profile = streamRow{
		Name:      prof.Name,
		Source:    firstSource(prof.Sources),
		Consumers: prof.Consumers,
	}
	s.renderAdminPage(w, "stream-edit", data)
}

func (s *Server) handleAdminStreamsUpdate(w http.ResponseWriter, r *http.Request) {
	if s.streams == nil {
		http.Error(w, "stream backend not configured", http.StatusServiceUnavailable)
		return
	}
	name := r.PathValue("name")
	if !streamProfileNameRE.MatchString(name) {
		http.Error(w, "ungueltiger Profil-Name", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	source := strings.TrimSpace(r.PostForm.Get("source"))
	if source == "" {
		http.Error(w, "Source-URL darf nicht leer sein.", http.StatusBadRequest)
		return
	}
	if err := s.streams.Put(r.Context(), name, []string{source}); err != nil {
		s.log.Error("admin streams update", "err", err, "name", name)
		http.Error(w, "Speichern fehlgeschlagen: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.log.Info("admin stream profile updated", "name", name)
	http.Redirect(w, r, "/a/streams", http.StatusSeeOther)
}

func (s *Server) handleAdminStreamsDelete(w http.ResponseWriter, r *http.Request) {
	if s.streams == nil {
		http.Error(w, "stream backend not configured", http.StatusServiceUnavailable)
		return
	}
	name := r.PathValue("name")
	if !streamProfileNameRE.MatchString(name) {
		http.Error(w, "ungueltiger Profil-Name", http.StatusBadRequest)
		return
	}
	if err := s.streams.Delete(r.Context(), name); err != nil {
		if errors.Is(err, streams.ErrProfileNotFound) {
			// Already gone - treat as success so double-clicks don't
			// paint a scary error.
			if wantsJSON(r) || r.Method == http.MethodDelete {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				_ = json.NewEncoder(w).Encode(map[string]any{"deleted": name, "note": "stale"})
				return
			}
			http.Redirect(w, r, "/a/streams", http.StatusSeeOther)
			return
		}
		s.log.Error("admin streams delete", "err", err, "name", name)
		http.Error(w, "Loeschen fehlgeschlagen: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.log.Info("admin stream profile deleted", "name", name)
	if wantsJSON(r) || r.Method == http.MethodDelete {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"deleted": name})
		return
	}
	http.Redirect(w, r, "/a/streams", http.StatusSeeOther)
}

func (s *Server) buildStreamsData(r *http.Request) adminStreamsData {
	username := AdminUserFromContext(r.Context())
	data := adminStreamsData{
		User:     adminUser{Name: username, Initials: initialsOf(username)},
		Defaults: adminStreamDefaults(),
	}
	if s.streams == nil {
		return data
	}
	data.Configured = true
	data.BackendURL = s.streams.BaseURL()
	profiles, err := s.streams.List(r.Context())
	if err != nil {
		s.log.Warn("admin streams list", "err", err)
		data.Flash = "go2rtc nicht erreichbar: " + err.Error()
		data.FlashType = "red"
		return data
	}
	for _, p := range profiles {
		data.Profiles = append(data.Profiles, streamRow{
			Name:      p.Name,
			Source:    firstSource(p.Sources),
			Consumers: p.Consumers,
		})
	}
	return data
}

func firstSource(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}
