// Admin CRUD against the stream-server profile registry.
//
// Read side renders /api/profiles for operator visibility, write
// side (S15-25) wires Edit / Create / Delete against the new
// snake_case PUT/DELETE endpoints. The per-viewer profile pick
// happens in the web-/ESP-viewer edit modal (fed by
// /a/streams.json).
//
// Routes (registered in server.go):
//
//	GET    /a/streams                 list-view (HTML)
//	GET    /a/streams.json            list payload as JSON (used
//	                                   by the viewer-edit modal
//	                                   stream-profile dropdown)
//	GET    /a/streams/new             create-form
//	GET    /a/streams/{name}          edit-form
//	POST   /a/streams                 create  (-> Client.Put)
//	POST   /a/streams/{name}          save    (-> Client.Put)
//	POST   /a/streams/{name}/delete   delete  (-> Client.Delete)
package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"carvilon.local/server/internal/streams"
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

// adminStreamEditData backs templates/admin/stream-edit.html.
// IsNew == true switches the template to the create variant
// (writable Name field, POST target /a/streams). PostError carries
// the stream-server's plain-text rejection so the operator sees
// exactly why a save was refused.
type adminStreamEditData struct {
	User      adminUser
	IsNew     bool
	Profile   streamRow
	PostError string
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
		data.Profiles = append(data.Profiles, profileToRow(p))
	}
	return data
}

// profileToRow flattens a streams.Profile to the row the list +
// edit templates render.
func profileToRow(p streams.Profile) streamRow {
	return streamRow{
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
	}
}

// handleAdminStreamNew renders the create-form (an empty edit
// template with IsNew == true). Defaults that map to the most
// common shape: codec mjpeg, usage esp, encryption tls.
func (s *Server) handleAdminStreamNew(w http.ResponseWriter, r *http.Request) {
	if !s.streamsConfiguredOr503(w) {
		return
	}
	username := AdminUserFromContext(r.Context())
	s.renderAdminPage(w, "stream-edit", adminStreamEditData{
		User:  adminUser{Name: username, Initials: initialsOf(username)},
		IsNew: true,
		Profile: streamRow{
			Codec:      "mjpeg",
			Usage:      "esp",
			Encryption: "tls",
		},
	})
}

// handleAdminStreamEdit renders the edit-form pre-filled from
// GET /api/profiles/{name}. A missing profile becomes a 404 with
// a hint-banner instead of a generic error.
func (s *Server) handleAdminStreamEdit(w http.ResponseWriter, r *http.Request) {
	if !s.streamsConfiguredOr503(w) {
		return
	}
	name := r.PathValue("name")
	p, err := s.streams.Get(r.Context(), name)
	if err != nil {
		if errors.Is(err, streams.ErrProfileNotFound) {
			http.Error(w, "Profil nicht gefunden.", http.StatusNotFound)
			return
		}
		s.log.Error("admin streams get", "err", err, "name", name)
		http.Error(w, "Stream-Backend nicht erreichbar: "+err.Error(),
			http.StatusBadGateway)
		return
	}
	username := AdminUserFromContext(r.Context())
	s.renderAdminPage(w, "stream-edit", adminStreamEditData{
		User:    adminUser{Name: username, Initials: initialsOf(username)},
		IsNew:   false,
		Profile: profileToRow(p),
	})
}

// handleAdminStreamSave persists an existing profile. Name comes
// from the path, the rest from the form. On validation failure
// the form is re-rendered with the stream-server's rejection
// message so the operator can fix and retry.
func (s *Server) handleAdminStreamSave(w http.ResponseWriter, r *http.Request) {
	if !s.streamsConfiguredOr503(w) {
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		http.Error(w, "Profil-Name fehlt.", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Formular kaputt.", http.StatusBadRequest)
		return
	}
	spec, formErr := parseStreamForm(r, name)
	if formErr != "" {
		s.rerenderStreamEdit(w, r, false, spec, formErr)
		return
	}
	if err := s.streams.Put(r.Context(), spec); err != nil {
		s.log.Warn("admin streams put", "err", err, "name", name)
		s.rerenderStreamEdit(w, r, false, spec, err.Error())
		return
	}
	http.Redirect(w, r, "/a/streams", http.StatusSeeOther)
}

// handleAdminStreamCreate persists a brand-new profile. Name
// comes from the form (the path is just /a/streams). The same
// PUT /api/profiles/{name} call serves both create and replace -
// the stream-server treats unknown names as upsert.
func (s *Server) handleAdminStreamCreate(w http.ResponseWriter, r *http.Request) {
	if !s.streamsConfiguredOr503(w) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Formular kaputt.", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("name"))
	spec, formErr := parseStreamForm(r, name)
	if formErr == "" && name == "" {
		formErr = "Profil-Name darf nicht leer sein."
	}
	if formErr != "" {
		s.rerenderStreamEdit(w, r, true, spec, formErr)
		return
	}
	if err := s.streams.Put(r.Context(), spec); err != nil {
		s.log.Warn("admin streams put (new)", "err", err, "name", name)
		s.rerenderStreamEdit(w, r, true, spec, err.Error())
		return
	}
	http.Redirect(w, r, "/a/streams", http.StatusSeeOther)
}

// handleAdminStreamDelete removes a profile via Client.Delete.
// 404 from the backend is treated as "already gone" and still
// redirects to the list without a flash.
func (s *Server) handleAdminStreamDelete(w http.ResponseWriter, r *http.Request) {
	if !s.streamsConfiguredOr503(w) {
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		http.Error(w, "Profil-Name fehlt.", http.StatusBadRequest)
		return
	}
	if err := s.streams.Delete(r.Context(), name); err != nil &&
		!errors.Is(err, streams.ErrProfileNotFound) {
		s.log.Warn("admin streams delete", "err", err, "name", name)
		http.Error(w, "Loeschen fehlgeschlagen: "+err.Error(),
			http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, "/a/streams", http.StatusSeeOther)
}

// parseStreamForm pulls the 11-field profile envelope out of the
// posted form. The returned spec is sent verbatim to Client.Put;
// the second return value carries a German operator-facing
// reason when input was rejected locally (allow-list violations
// on usage / codec / encryption, malformed numbers).
//
// Numbers default to zero on empty input; the stream-server
// tolerates 0 for the non-MJPEG profiles. Trimming runs on every
// string field so trailing whitespace does not slip in.
func parseStreamForm(r *http.Request, name string) (streams.Profile, string) {
	codec := strings.TrimSpace(r.PostForm.Get("codec"))
	if !validStreamCodec(codec) {
		return streams.Profile{}, "Ungueltiger Codec (mjpeg / h264_cbp / h264_passthrough)."
	}
	usage := strings.TrimSpace(r.PostForm.Get("usage"))
	if !validStreamUsage(usage) {
		return streams.Profile{}, "Ungueltige Nutzung (esp / browser)."
	}
	encryption := strings.TrimSpace(r.PostForm.Get("encryption"))
	if encryption == "" {
		encryption = "tls"
	}
	if !validStreamEncryption(encryption) {
		return streams.Profile{}, "Ungueltiger Verschluesselungs-Modus (tls / srtp)."
	}
	width, ok := parseStreamInt(r.PostForm.Get("width"))
	if !ok {
		return streams.Profile{}, "Width muss eine Zahl sein."
	}
	height, ok := parseStreamInt(r.PostForm.Get("height"))
	if !ok {
		return streams.Profile{}, "Height muss eine Zahl sein."
	}
	fps, ok := parseStreamInt(r.PostForm.Get("fps"))
	if !ok {
		return streams.Profile{}, "FPS muss eine Zahl sein."
	}
	encQ, ok := parseStreamInt(r.PostForm.Get("encode_quality"))
	if !ok {
		return streams.Profile{}, "Encode-Quality muss eine Zahl sein."
	}
	return streams.Profile{
		Name:          name,
		CameraID:      strings.TrimSpace(r.PostForm.Get("camera_id")),
		Quality:       strings.TrimSpace(r.PostForm.Get("quality")),
		Usage:         usage,
		Description:   strings.TrimSpace(r.PostForm.Get("description")),
		Codec:         codec,
		Width:         width,
		Height:        height,
		FPS:           fps,
		EncodeQuality: encQ,
		Encryption:    encryption,
	}, ""
}

// rerenderStreamEdit shows the edit form with the just-typed
// values + a German error banner. isNew preserves the create-
// vs-save distinction across the redraw.
func (s *Server) rerenderStreamEdit(w http.ResponseWriter, r *http.Request, isNew bool, spec streams.Profile, errMsg string) {
	username := AdminUserFromContext(r.Context())
	s.renderAdminPage(w, "stream-edit", adminStreamEditData{
		User:      adminUser{Name: username, Initials: initialsOf(username)},
		IsNew:     isNew,
		Profile:   profileToRow(spec),
		PostError: errMsg,
	})
}

// streamsConfiguredOr503 fails closed when no backend is wired
// so the write handlers do not produce confusing
// ErrNotConfigured banners pretending the save worked.
func (s *Server) streamsConfiguredOr503(w http.ResponseWriter) bool {
	if s.streams.Configured() {
		return true
	}
	http.Error(w, "Stream-Backend nicht konfiguriert.", http.StatusServiceUnavailable)
	return false
}

func validStreamCodec(c string) bool {
	switch c {
	case "mjpeg", "h264_cbp", "h264_passthrough":
		return true
	}
	return false
}

func validStreamUsage(u string) bool {
	switch u {
	case "esp", "browser":
		return true
	}
	return false
}

func validStreamEncryption(e string) bool {
	switch e {
	case "tls", "srtp":
		return true
	}
	return false
}

// parseStreamInt reads a form-field integer. Empty is allowed and
// becomes zero; anything else must parse cleanly.
func parseStreamInt(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, true
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}
