package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"carvilon.local/server/internal/access"
	"carvilon.local/server/internal/platformconfig"
)

const (
	usersDefaultPageSize = 20
	usersMaxPageSize     = 100
)

// adminUsersData is the payload for templates/admin/users.html.
//
// CARVILON ist die Master-Datenbank: es gibt genau EINE Benutzerliste
// (NativeUsers). Ein UA-Profil ist kein eigener Eintrag, sondern nur
// eine optionale Verknuepfung (ua_user_id) an einem CARVILON-Benutzer.
// Wenn UA aktiv + erreichbar ist, wird pro Benutzer eine Verknuepfen-/
// Loesen-Aktion angeboten; die UA-Profile erscheinen NUR im
// Verknuepfen-Dialog als Auswahl (UACandidates), nie als Liste.
type adminUsersData struct {
	User adminUser

	// Die einzige Benutzerliste (access/carvilon, Migration 034).
	NativeUsers []nativeUserRow
	NativeTotal int
	NativeQuery string

	// UA nur als Verknuepfungs-Bruecke:
	UAEnabled    bool          // "UA aktiv"-Schalter an
	UAConfigured bool          // zusaetzlich Backend erreichbar (Token/URL)
	UAError      string        // gesetzt, wenn UA an+konfiguriert, aber der Profil-Abruf scheiterte
	UACandidates []uaCandidate // UA-Profile fuer den (geteilten) Verknuepfen-Dialog

	Flash     string
	FlashType string
}

// nativeUserRow is one row in the CARVILON-users table view.
type nativeUserRow struct {
	ID          string
	DisplayName string
	Initials    string
	Active      bool
	StatusLabel string // "Aktiv" | "Inaktiv"
	// Optionale UA-Verknuepfung (nur ein Attribut, kein eigener Nutzer):
	UALinked     bool   // hat eine UA-Kopplung
	LinkedUAID   string // die ua_user_id (opak)
	LinkedUAName string // aufgeloester UA-Profil-Anzeigename (oder Fallback)
}

// uaCandidate ist ein UA-Profil, das im Verknuepfen-Dialog zur Auswahl
// steht. LinkedToName ist der CARVILON-Benutzer, an dem das Profil schon
// haengt (leer = frei, sonst wird die Option gesperrt).
type uaCandidate struct {
	ID           string
	Name         string
	LinkedToName string
}

type userRow struct {
	ID             string
	DisplayName    string
	Initials       string
	Email          string
	EmployeeNumber string
	Status         string // "active" | "deactivated" | "pending"
	StatusLabel    string // "Aktiv" | "Inaktiv" | "Pending"
	HasNFC         bool
	HasPIN         bool
}

// adminUserDetailData is the payload for /a/users/{id}.
type adminUserDetailData struct {
	User          adminUser
	Configured    bool
	Profile       userRow
	LinkedViewers []linkedViewerRow
	NotFoundFlash string
	Flash         string
	FlashType     string
}

type linkedViewerRow struct {
	MAC    string
	Name   string
	Online bool
}

// handleAdminUsersList renders /a/users: the single CARVILON user list.
// When UA is enabled and reachable, each user gets a "UA-Profil
// verknuepfen"/"Loesen" action and the UA profiles are offered as a
// selection (never as a second list).
func (s *Server) handleAdminUsersList(w http.ResponseWriter, r *http.Request) {
	username := AdminUserFromContext(r.Context())
	data := adminUsersData{
		User:        adminUser{Name: username, Initials: initialsOf(username)},
		NativeQuery: strings.TrimSpace(r.URL.Query().Get("nq")),
	}

	// The (optionally filtered) list to display.
	var displayList []access.NativeUser
	if s.nativeUsers != nil {
		list, err := s.nativeUsers.List(r.Context(), access.NativeListParams{Query: data.NativeQuery})
		if err != nil {
			s.log.Warn("native users list failed", "err", err)
			data.Flash = "Eigene Benutzer konnten nicht geladen werden."
			data.FlashType = "red"
		} else {
			displayList = list
			data.NativeTotal = len(list)
		}
	}

	// linkOwner maps ua_user_id -> owning CARVILON display name, built
	// from the FULL set (unfiltered) so the candidate dialog knows every
	// existing link even under an active search filter.
	linkOwner := map[string]string{}
	fullList := displayList
	if s.nativeUsers != nil && data.NativeQuery != "" {
		if all, err := s.nativeUsers.List(r.Context(), access.NativeListParams{}); err == nil {
			fullList = all
		}
	}
	for _, u := range fullList {
		if u.UAUserID != "" {
			linkOwner[u.UAUserID] = u.DisplayName
		}
	}

	// UA is a bridge only: resolve profile names + build the candidate
	// list for the link dialog. Nothing UA-related when the toggle is off.
	data.UAEnabled = s.uaEnabled(r.Context())
	uaNames := map[string]string{}
	if data.UAEnabled && s.userStore != nil && s.userStore.IsConfigured() {
		data.UAConfigured = true
		profiles, err := s.listAllUAProfiles(r.Context())
		if err != nil {
			s.log.Warn("ua profiles fetch failed", "err", err)
			data.UAError = friendlyAccessError(err)
		} else {
			for _, p := range profiles {
				name := p.DisplayName()
				uaNames[p.ID] = name
				data.UACandidates = append(data.UACandidates, uaCandidate{
					ID: p.ID, Name: name, LinkedToName: linkOwner[p.ID],
				})
			}
		}
	}

	for _, u := range displayList {
		row := toNativeUserRow(u)
		if u.UAUserID != "" {
			switch {
			case uaNames[u.UAUserID] != "":
				row.LinkedUAName = uaNames[u.UAUserID]
			case data.UAConfigured && data.UAError == "":
				// UA reachable but this id has no profile -> dangling link.
				row.LinkedUAName = "UA-Profil " + u.UAUserID + " (nicht gefunden)"
			default:
				// UA off or unreachable: show the raw id, don't guess a name.
				row.LinkedUAName = "UA-Profil " + u.UAUserID
			}
		}
		data.NativeUsers = append(data.NativeUsers, row)
	}
	s.renderAdminPage(w, "users", data)
}

// listAllUAProfiles pulls the full UA user list (paging through the
// store's 100-cap) for the link dialog + name resolution. Realistic
// staff counts are small; the guard bounds a pathological case.
func (s *Server) listAllUAProfiles(ctx context.Context) ([]access.User, error) {
	var all []access.User
	for page := 1; page <= 1000; page++ {
		res, err := s.userStore.List(ctx, access.ListParams{Page: page, Size: usersMaxPageSize})
		if err != nil {
			return nil, err
		}
		all = append(all, res.Users...)
		if len(res.Users) == 0 || len(all) >= res.Total {
			break
		}
	}
	return all, nil
}

// uaEnabled resolves the effective "UA aktiv" toggle for the Benutzer
// page. An explicit "1"/"0" stored under KeyUAEnabled wins; if unset
// (fresh install) the default is on when a UA token is stored, off
// otherwise. This is the single gate for every UA-related bit of the
// Benutzer page - and only that page.
func (s *Server) uaEnabled(ctx context.Context) bool {
	if s.platformCfg == nil {
		return false
	}
	switch raw, _ := s.platformCfg.Get(ctx, platformconfig.KeyUAEnabled); raw {
	case "1":
		return true
	case "0":
		return false
	default:
		tok, _ := s.platformCfg.GetSecret(ctx, platformconfig.KeyUAAPIToken)
		return strings.TrimSpace(tok) != ""
	}
}

// uaAvailable is true when the UA section is both enabled and backed
// by a configured store - the precondition for any UA proxy call from
// the Benutzer page.
func (s *Server) uaAvailable(ctx context.Context) bool {
	return s.uaEnabled(ctx) && s.userStore != nil && s.userStore.IsConfigured()
}

func toNativeUserRow(u access.NativeUser) nativeUserRow {
	label := "Inaktiv"
	if u.Active {
		label = "Aktiv"
	}
	return nativeUserRow{
		ID:          u.ID,
		DisplayName: u.DisplayName,
		Initials:    u.Initials(),
		Active:      u.Active,
		StatusLabel: label,
		UALinked:    u.UAUserID != "",
		LinkedUAID:  u.UAUserID,
		// LinkedUAName wird im Handler aufgeloest (braucht die UA-Namen).
	}
}

// handleAdminUsersListJSON returns the same list as JSON. Used
// for AJAX pagination and for the dropdown in the web-viewer
// create modal.
func (s *Server) handleAdminUsersListJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if s.userStore == nil || !s.userStore.IsConfigured() {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"configured": false,
			"users":      []any{},
			"total":      0,
		})
		return
	}
	params := parseListParams(r)
	res, err := s.userStore.List(r.Context(), params)
	if err != nil {
		s.log.Warn("users.json failed", "err", err)
		http.Error(w, friendlyAccessError(err), accessErrorStatus(err))
		return
	}
	rows := make([]userRow, 0, len(res.Users))
	for _, u := range res.Users {
		rows = append(rows, toUserRow(u))
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"configured": true,
		"users":      rows,
		"total":      res.Total,
		"page":       params.Page,
		"size":       params.Size,
		"page_count": pageCount(res.Total, params.Size),
	})
}

// handleAdminUsersDetail renders /a/users/{id}.
func (s *Server) handleAdminUsersDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	username := AdminUserFromContext(r.Context())
	data := adminUserDetailData{
		User:       adminUser{Name: username, Initials: initialsOf(username)},
		Configured: s.uaAvailable(r.Context()),
	}
	if !data.Configured {
		s.renderAdminPage(w, "user-detail", data)
		return
	}
	u, err := s.userStore.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, access.ErrNotFound) {
			data.NotFoundFlash = "Benutzer wurde nicht gefunden (eventuell zwischendurch geloescht)."
		} else {
			data.Flash = friendlyAccessError(err)
			data.FlashType = "red"
		}
		s.renderAdminPage(w, "user-detail", data)
		return
	}
	data.Profile = toUserRow(u)
	data.LinkedViewers = s.collectLinkedViewers(r, id)
	s.renderAdminPage(w, "user-detail", data)
}

// Hinweis: UA-Profile werden NICHT mehr ueber uns angelegt/bearbeitet/
// geloescht (Korrektur-Modell: UA ist kein eigener Benutzer-Bestand,
// nur eine Verknuepfung). Die frueheren UA-CRUD-Handler sind entfallen;
// geblieben sind der read-only Profil-Blick (handleAdminUsersDetail,
// verlinkt aus den Viewer-Seiten) und die Profil-Liste als JSON fuer
// den Viewer-Verknuepfungs-Dialog (handleAdminUsersListJSON).

// --- helpers ---

// parseListParams reads page / size / q / status from the URL.
func parseListParams(r *http.Request) access.ListParams {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	size, _ := strconv.Atoi(q.Get("size"))
	if size <= 0 {
		size = usersDefaultPageSize
	}
	if size > usersMaxPageSize {
		size = usersMaxPageSize
	}
	statusFilter := access.Status(strings.ToLower(q.Get("status")))
	switch statusFilter {
	case access.StatusActive, access.StatusDeactivated, access.StatusPending:
		// known status, keep as is
	default:
		statusFilter = ""
	}
	return access.ListParams{
		Page:         page,
		Size:         size,
		Query:        strings.TrimSpace(q.Get("q")),
		StatusFilter: statusFilter,
	}
}

func pageCount(total, size int) int {
	if size <= 0 || total <= 0 {
		return 0
	}
	if total%size == 0 {
		return total / size
	}
	return total/size + 1
}

func toUserRow(u access.User) userRow {
	return userRow{
		ID:             u.ID,
		DisplayName:    u.DisplayName(),
		Initials:       u.Initials(),
		Email:          u.Email,
		EmployeeNumber: u.EmployeeNumber,
		Status:         string(u.Status),
		StatusLabel:    statusLabelGerman(u.Status),
		HasNFC:         u.HasNFC,
		HasPIN:         u.HasPIN,
	}
}

func statusLabelGerman(s access.Status) string {
	switch s {
	case access.StatusActive:
		return "Aktiv"
	case access.StatusDeactivated:
		return "Inaktiv"
	case access.StatusPending:
		return "Pending"
	default:
		return "?"
	}
}

func friendlyAccessError(err error) string {
	switch {
	case errors.Is(err, access.ErrUnauthorized):
		return "UA-API-Token ungueltig. Bitte unter Einstellungen pruefen."
	case errors.Is(err, access.ErrNotFound):
		return "Benutzer wurde nicht gefunden."
	case errors.Is(err, access.ErrNotConfigured):
		return "UA-API noch nicht konfiguriert."
	default:
		return fmt.Sprintf("UA-Fehler: %v", err)
	}
}

func accessErrorStatus(err error) int {
	switch {
	case errors.Is(err, access.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, access.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, access.ErrNotConfigured):
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadGateway
	}
}

// collectLinkedViewers loads the web viewers whose
// linked_ua_user_id points at this UA user.
func (s *Server) collectLinkedViewers(r *http.Request, userID string) []linkedViewerRow {
	if s.viewerMgr == nil || userID == "" {
		return nil
	}
	all, err := s.viewerMgr.ListViewers(r.Context())
	if err != nil {
		s.log.Warn("linked viewers list failed", "err", err)
		return nil
	}
	out := make([]linkedViewerRow, 0)
	for _, v := range all {
		if v.LinkedUAUserID == userID {
			out = append(out, linkedViewerRow{
				MAC:    v.MAC,
				Name:   v.Name,
				Online: v.Running,
			})
		}
	}
	return out
}
