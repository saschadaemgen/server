// Saison 14-01: admin CRUD for stream profiles.
// Saison 15-01: surface migrated behind the streams.StreamBackend
// seam; profile CRUD is transitional.
//
// The page lives under /a/streams and renders against whichever
// backend the operator has wired. With the public-build default
// (Unconfigured), the page renders a "kein Stream-Backend wired"
// banner. With the transitional go2rtc client, List/Get/Delete
// still work; Put returns ErrNotConfigured because profile CRUD
// moves to the carvilon-streaming-server (a structured form
// lands once that backend is plugged in).
//
// Routes (registered in server.go):
//
//	GET    /a/streams                 list-view (HTML)
//	GET    /a/streams.json            list payload as JSON (used
//	                                   by the viewer-edit modal
//	                                   stream-profile dropdown)
//	POST   /a/streams                 create (form: name, source)
//	GET    /a/streams/{name}          edit-view (HTML, single)
//	POST   /a/streams/{name}          update (form: source)
//	POST   /a/streams/{name}/delete   delete (form)
//	DELETE /a/streams/{name}          delete (JSON / REST flavour)
package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"carvilon.local/server/internal/streams"
)

// streamProfileNameRE locks profile names down to a safe alphabet.
// go2rtc itself accepts looser names but our admin UI passes them
// through URL paths and dropdowns, so we keep it boring.
var streamProfileNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// validStreamSchemes is the allow-list every Source-URL must
// match. Saison 14-01-FIX04: switch from a generic "no spaces"
// rule (which never existed in carvilon-code but is enforced by
// go2rtc) to a scheme-aware check so the ffmpeg:-syntax can
// carry literal spaces in its raw=-argument list.
var validStreamSchemes = []string{
	"rtsp://", "rtsps://", "rtspx://",
	"http://", "https://",
	"ffmpeg:", "exec:",
	"tcp://", "udp://",
	"onvif://", "homekit://",
}

// validateStreamSource checks a go2rtc source URL against the
// scheme allow-list.
//
//   - empty / whitespace-only: rejected.
//   - prefix not in validStreamSchemes: rejected (catches things
//     like "javascript:...", "file://", typos like "rtps://").
//   - ffmpeg:-prefix: accepted as-is. The DSL after the prefix
//     uses `#`-separated key=value pairs; the raw= value carries
//     literal ffmpeg arguments separated by spaces, which are
//     therefore allowed.
//   - everything else (rtsp://, http://, ...): rejected if any
//     whitespace is present. An unencoded space in a URL is
//     almost always a typo or a smuggling attempt.
//
// The check is intentionally shallow - go2rtc handles the deep
// validation when it tries to dial the source. We only catch
// obvious mistakes early so the admin UI flash carries a
// German-language hint instead of an opaque go2rtc 400.
func validateStreamSource(src string) error {
	src = strings.TrimSpace(src)
	if src == "" {
		return errors.New("Source-URL darf nicht leer sein")
	}
	hasValidPrefix := false
	for _, p := range validStreamSchemes {
		if strings.HasPrefix(src, p) {
			hasValidPrefix = true
			break
		}
	}
	if !hasValidPrefix {
		return errors.New("Source-URL muss mit einem bekannten Schema beginnen " +
			"(rtsp://, rtsps://, rtspx://, http://, https://, " +
			"ffmpeg:, exec:, tcp://, udp://, onvif://, homekit://)")
	}
	if strings.HasPrefix(src, "ffmpeg:") {
		return nil
	}
	if strings.ContainsAny(src, " \t\n\r") {
		return errors.New("Source-URL enthaelt Whitespace; " +
			"fuer ffmpeg-Argument-Listen die ffmpeg:-Syntax verwenden, " +
			"sonst URL-encoden")
	}
	return nil
}

// rewriteStreamBackendError converts the most common stream-
// backend error messages into a German-language hint so the
// admin UI flash is useful out of the box.
//
// Saison 15-01: ErrNotConfigured from Put is the migration
// signal. The transitional go2rtc client stubs Put because
// profile CRUD moves to the carvilon-streaming-server; the new
// structured form lands once that backend is wired. Until then
// the operator gets a clear hint instead of a generic 502.
//
// The legacy "source with spaces may be insecure" mapping from
// the go2rtc PUT path stays in place for the historical Update
// surface even though Put itself no longer reaches go2rtc - the
// rule still applies for any backend that accepts free-form
// source URLs.
func rewriteStreamBackendError(err error) string {
	if errors.Is(err, streams.ErrNotConfigured) {
		return "Profil-CRUD ist auf den eigenen carvilon-streaming-server umgezogen; " +
			"das strukturierte Formular (Kamera, Qualitaet, Nutzung) kommt mit dem naechsten Update."
	}
	msg := err.Error()
	if strings.Contains(msg, "source with spaces may be insecure") {
		return "Stream-Backend lehnt Source-URLs mit Leerzeichen ueber die REST-API ab " +
			"(Sicherheits-Check). Workaround: Backend-Config manuell editieren und den " +
			"Dienst neu starten."
	}
	return msg
}

// adminStreamsData is the payload for templates/admin/streams.html.
type adminStreamsData struct {
	User       adminUser
	Configured bool   // false = no go2rtc backend URL set
	BackendURL string // for the "go2rtc API:" hint line
	Profiles   []streamRow
	Flash      string
	FlashType  string
	Defaults   []streamDefault // template buttons in the create-modal
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
			Label:       "Browser-Default (900x1200, 15 FPS, q:v 4)",
			Source:      "ffmpeg:intercom_high#video=mjpeg#width=900#height=1200#raw=-r 15 -q:v 4",
			Description: "Mieter-Browser. UA-Intercom liefert nativ 1200x1600; das hier ist ein leicht beschnittenes Profil bei guter Qualitaet.",
		},
		{
			Name:   "intercom_high",
			Label:  "Hochaufloesend (Source-Profil, normalerweise in go2rtc.yaml)",
			Source: "rtsps://<udm-ip>:7441/<token>",
			Description: "Source-Profil von dem ESP- und Browser-Profile abgeleitet werden. " +
				"Wird normalerweise EINMAL beim Setup direkt in go2rtc.yaml angelegt; " +
				"go2rtc lehnt RTSPS-URLs mit Token via REST-API ab (Sicherheits-Check). " +
				"Nur als Referenz/Vorlage hier.",
		},
	}
}

// streamBackendBaseURL is a soft accessor for the operator-facing
// "API: <url>" hint. The StreamBackend interface intentionally
// has no BaseURL method (the seam exposes URLs only via
// MJPEGURL/WebRTCSignalURL); the admin UI just wants a display
// string for the banner. We type-assert against the concrete
// go2rtc Client for now; the future commercial backend can grow
// the same accessor when it lands. Unknown backends return "".
type baseURLer interface{ BaseURL() string }

func streamBackendBaseURL(b streams.StreamBackend) string {
	if u, ok := b.(baseURLer); ok {
		return u.BaseURL()
	}
	return ""
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
	if !s.streams.Configured() {
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
	if err := validateStreamSource(source); err != nil {
		http.Error(w, err.Error()+".", http.StatusBadRequest)
		return
	}
	// Saison 15-01: the new Put signature takes a structured
	// Profile. Until the structured admin form lands (follow-up
	// briefing), we park the legacy source string in Description
	// so the new streaming server can later parse it back. The
	// transitional go2rtc client returns ErrNotConfigured here -
	// rewriteStreamBackendError surfaces the migration hint.
	prof := streams.Profile{Name: name, Description: source}
	if err := s.streams.Put(r.Context(), prof); err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, streams.ErrNotConfigured) {
			status = http.StatusServiceUnavailable
		} else {
			s.log.Error("admin streams create", "err", err, "name", name)
		}
		http.Error(w, "Anlegen fehlgeschlagen: "+rewriteStreamBackendError(err), status)
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
	if !s.streams.Configured() {
		data.Configured = false
		data.Profile = streamRow{Name: name}
		s.renderAdminPage(w, "stream-edit", data)
		return
	}
	data.Configured = true
	data.BackendURL = streamBackendBaseURL(s.streams)
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
		Source:    prof.Description,
		Consumers: prof.Consumers,
	}
	s.renderAdminPage(w, "stream-edit", data)
}

func (s *Server) handleAdminStreamsUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.streams.Configured() {
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
	if err := validateStreamSource(source); err != nil {
		http.Error(w, err.Error()+".", http.StatusBadRequest)
		return
	}
	// Saison 15-01: same transitional pattern as Create. Once the
	// structured form lands, the source string is replaced by the
	// CameraID/Quality/Usage triple.
	prof := streams.Profile{Name: name, Description: source}
	if err := s.streams.Put(r.Context(), prof); err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, streams.ErrNotConfigured) {
			status = http.StatusServiceUnavailable
		} else {
			s.log.Error("admin streams update", "err", err, "name", name)
		}
		http.Error(w, "Speichern fehlgeschlagen: "+rewriteStreamBackendError(err), status)
		return
	}
	s.log.Info("admin stream profile updated", "name", name)
	http.Redirect(w, r, "/a/streams", http.StatusSeeOther)
}

func (s *Server) handleAdminStreamsDelete(w http.ResponseWriter, r *http.Request) {
	if !s.streams.Configured() {
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
	if !s.streams.Configured() {
		return data
	}
	data.Configured = true
	data.BackendURL = streamBackendBaseURL(s.streams)
	profiles, err := s.streams.List(r.Context())
	if err != nil {
		s.log.Warn("admin streams list", "err", err)
		data.Flash = "Stream-Backend nicht erreichbar: " + err.Error()
		data.FlashType = "red"
		return data
	}
	for _, p := range profiles {
		data.Profiles = append(data.Profiles, streamRow{
			Name:      p.Name,
			Source:    p.Description,
			Consumers: p.Consumers,
		})
	}
	return data
}
