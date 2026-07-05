package httpserver

import (
	"errors"
	"net/http"
	"strings"

	"carvilon.local/server/internal/access"
)

// Native CARVILON user handlers. These manage CARVILONs own users
// (access/carvilon, migration 034) - the canonical user source,
// independent of UA. All redirect back to the unified /a/users page.

// handleAdminNativeUserCreate handles POST /a/users/carvilon.
func (s *Server) handleAdminNativeUserCreate(w http.ResponseWriter, r *http.Request) {
	if s.nativeUsers == nil {
		http.Error(w, "Benutzerverwaltung nicht verfuegbar.", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "ungueltige Formular-Daten.", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("display_name"))
	if name == "" {
		http.Error(w, "Anzeigename ist Pflicht.", http.StatusBadRequest)
		return
	}
	if _, err := s.nativeUsers.Create(r.Context(), access.CreateNativeUserParams{DisplayName: name}); err != nil {
		s.log.Warn("native user create failed", "err", err)
		http.Error(w, "Benutzer konnte nicht angelegt werden.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/a/users", http.StatusSeeOther)
}

// handleAdminNativeUserUpdate handles POST /a/users/carvilon/{id}/update.
func (s *Server) handleAdminNativeUserUpdate(w http.ResponseWriter, r *http.Request) {
	if s.nativeUsers == nil {
		http.Error(w, "Benutzerverwaltung nicht verfuegbar.", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "ungueltige Formular-Daten.", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("display_name"))
	if name == "" {
		http.Error(w, "Anzeigename ist Pflicht.", http.StatusBadRequest)
		return
	}
	if _, err := s.nativeUsers.Update(r.Context(), id, access.UpdateNativeUserParams{DisplayName: name}); err != nil {
		s.handleNativeUserError(w, "update", err)
		return
	}
	http.Redirect(w, r, "/a/users", http.StatusSeeOther)
}

// handleAdminNativeUserActivate / Deactivate flip the Aktiv flag.
func (s *Server) handleAdminNativeUserActivate(w http.ResponseWriter, r *http.Request) {
	s.setNativeUserActive(w, r, true)
}

func (s *Server) handleAdminNativeUserDeactivate(w http.ResponseWriter, r *http.Request) {
	s.setNativeUserActive(w, r, false)
}

func (s *Server) setNativeUserActive(w http.ResponseWriter, r *http.Request, active bool) {
	if s.nativeUsers == nil {
		http.Error(w, "Benutzerverwaltung nicht verfuegbar.", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	if err := s.nativeUsers.SetActive(r.Context(), id, active); err != nil {
		s.handleNativeUserError(w, "set active", err)
		return
	}
	http.Redirect(w, r, "/a/users", http.StatusSeeOther)
}

// handleAdminNativeUserDelete handles POST /a/users/carvilon/{id}/delete.
func (s *Server) handleAdminNativeUserDelete(w http.ResponseWriter, r *http.Request) {
	if s.nativeUsers == nil {
		http.Error(w, "Benutzerverwaltung nicht verfuegbar.", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	if err := s.nativeUsers.Delete(r.Context(), id); err != nil {
		s.handleNativeUserError(w, "delete", err)
		return
	}
	http.Redirect(w, r, "/a/users", http.StatusSeeOther)
}

// handleAdminNativeUserLink handles POST /a/users/carvilon/{id}/link -
// heftet ein UA-Profil (ua_user_id) an den CARVILON-Benutzer. Das
// UA-Profil ist kein eigener Nutzer, nur diese Verknuepfung. Erfordert
// aktives + erreichbares UA (sonst gibt es keine Profile zum Waehlen).
func (s *Server) handleAdminNativeUserLink(w http.ResponseWriter, r *http.Request) {
	if s.nativeUsers == nil {
		http.Error(w, "Benutzerverwaltung nicht verfuegbar.", http.StatusServiceUnavailable)
		return
	}
	if !s.uaAvailable(r.Context()) {
		http.Error(w, "UA ist deaktiviert oder nicht konfiguriert.", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "ungueltige Formular-Daten.", http.StatusBadRequest)
		return
	}
	uaUserID := strings.TrimSpace(r.PostForm.Get("ua_user_id"))
	if uaUserID == "" {
		http.Error(w, "Kein UA-Profil gewaehlt.", http.StatusBadRequest)
		return
	}
	switch err := s.nativeUsers.SetUALink(r.Context(), id, uaUserID); {
	case errors.Is(err, access.ErrUALinkTaken):
		http.Error(w, "Dieses UA-Profil ist bereits mit einem anderen Benutzer verknuepft.", http.StatusConflict)
		return
	case errors.Is(err, access.ErrNotFound):
		http.Error(w, "Benutzer wurde nicht gefunden.", http.StatusNotFound)
		return
	case err != nil:
		s.log.Warn("native user link failed", "err", err)
		http.Error(w, "Verknuepfung fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/a/users", http.StatusSeeOther)
}

// handleAdminNativeUserUnlink handles POST /a/users/carvilon/{id}/unlink -
// loest die UA-Verknuepfung (ua_user_id -> NULL). Braucht kein aktives
// UA (eine stale Verknuepfung soll sich immer entfernen lassen).
func (s *Server) handleAdminNativeUserUnlink(w http.ResponseWriter, r *http.Request) {
	if s.nativeUsers == nil {
		http.Error(w, "Benutzerverwaltung nicht verfuegbar.", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	if err := s.nativeUsers.SetUALink(r.Context(), id, ""); err != nil {
		s.handleNativeUserError(w, "unlink", err)
		return
	}
	http.Redirect(w, r, "/a/users", http.StatusSeeOther)
}

// handleNativeUserError maps a store error to an HTTP response. A
// missing row is a 404 (the user was likely removed concurrently);
// everything else is a 500 with a logged cause.
func (s *Server) handleNativeUserError(w http.ResponseWriter, op string, err error) {
	if errors.Is(err, access.ErrNotFound) {
		http.Error(w, "Benutzer wurde nicht gefunden.", http.StatusNotFound)
		return
	}
	s.log.Warn("native user "+op+" failed", "err", err)
	http.Error(w, "Aktion fehlgeschlagen.", http.StatusInternalServerError)
}
