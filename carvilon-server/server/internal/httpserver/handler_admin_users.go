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

// adminUsersData is the payload for templates/admin/users.html - the
// unified Benutzer page. CARVILONs own users (Native*) are always
// shown with full management; the UA section (UA*) only renders when
// UA is enabled.
type adminUsersData struct {
	User adminUser

	// Native CARVILON users (access/carvilon, migration 034). Always
	// present; this is the canonical user source.
	NativeUsers []nativeUserRow
	NativeTotal int
	NativeQuery string

	// UA section. UAEnabled is the effective "UA aktiv" toggle;
	// UAConfigured additionally requires a stored token/base URL.
	UAEnabled    bool
	UAConfigured bool
	Users        []userRow
	Total        int
	Page         int
	PageSize     int
	PageCount    int
	Prev         int
	Next         int
	Query        string
	StatusFilter string

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
	UALinked    bool   // has an optional UA link (info only for now)
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

// handleAdminUsersList renders the unified /a/users page: CARVILONs
// own users (always, full management) plus - only when UA is enabled -
// the UA proxy list as a clearly separated section.
func (s *Server) handleAdminUsersList(w http.ResponseWriter, r *http.Request) {
	username := AdminUserFromContext(r.Context())
	data := adminUsersData{
		User:     adminUser{Name: username, Initials: initialsOf(username)},
		Page:     1,
		PageSize: usersDefaultPageSize,
	}

	// --- Native CARVILON users (always) ---
	data.NativeQuery = strings.TrimSpace(r.URL.Query().Get("nq"))
	if s.nativeUsers != nil {
		list, err := s.nativeUsers.List(r.Context(), access.NativeListParams{Query: data.NativeQuery})
		if err != nil {
			s.log.Warn("native users list failed", "err", err)
			data.Flash = "Eigene Benutzer konnten nicht geladen werden."
			data.FlashType = "red"
		} else {
			for _, u := range list {
				data.NativeUsers = append(data.NativeUsers, toNativeUserRow(u))
			}
			data.NativeTotal = len(list)
		}
	}

	// --- UA users (only when UA is enabled) ---
	data.UAEnabled = s.uaEnabled(r.Context())
	if data.UAEnabled {
		data.UAConfigured = s.userStore != nil && s.userStore.IsConfigured()
		if data.UAConfigured {
			params := parseListParams(r)
			data.Page = params.Page
			data.PageSize = params.Size
			data.Query = params.Query
			data.StatusFilter = string(params.StatusFilter)

			res, err := s.userStore.List(r.Context(), params)
			if err != nil {
				s.log.Warn("users list failed", "err", err)
				data.Flash = friendlyAccessError(err)
				data.FlashType = "red"
			} else {
				data.Total = res.Total
				data.PageCount = pageCount(res.Total, params.Size)
				if params.Page > 1 {
					data.Prev = params.Page - 1
				}
				if params.Page < data.PageCount {
					data.Next = params.Page + 1
				}
				for _, u := range res.Users {
					data.Users = append(data.Users, toUserRow(u))
				}
			}
		}
	}
	s.renderAdminPage(w, "users", data)
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

// handleAdminUsersCreate handles POST /a/users (create via the
// modal form or a direct POST).
func (s *Server) handleAdminUsersCreate(w http.ResponseWriter, r *http.Request) {
	if !s.uaAvailable(r.Context()) {
		http.Error(w, "UA ist deaktiviert oder nicht konfiguriert.", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "ungueltige Formular-Daten.", http.StatusBadRequest)
		return
	}
	params := access.CreateUserParams{
		FirstName:      strings.TrimSpace(r.PostForm.Get("first_name")),
		LastName:       strings.TrimSpace(r.PostForm.Get("last_name")),
		Email:          strings.TrimSpace(r.PostForm.Get("email")),
		EmployeeNumber: strings.TrimSpace(r.PostForm.Get("employee_number")),
	}
	if params.FirstName == "" && params.LastName == "" {
		http.Error(w, "Vor- oder Nachname ist Pflicht.", http.StatusBadRequest)
		return
	}
	created, err := s.userStore.Create(r.Context(), params)
	if err != nil {
		s.log.Warn("user create failed", "err", err)
		http.Error(w, friendlyAccessError(err), accessErrorStatus(err))
		return
	}
	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(toUserRow(created))
		return
	}
	http.Redirect(w, r, "/a/users/"+created.ID, http.StatusSeeOther)
}

// handleAdminUsersUpdate handles POST /a/users/{id}/update.
func (s *Server) handleAdminUsersUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.uaAvailable(r.Context()) {
		http.Error(w, "UA ist deaktiviert oder nicht konfiguriert.", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "ungueltige Formular-Daten.", http.StatusBadRequest)
		return
	}
	_, err := s.userStore.Update(r.Context(), id, access.UpdateUserParams{
		FirstName:      strings.TrimSpace(r.PostForm.Get("first_name")),
		LastName:       strings.TrimSpace(r.PostForm.Get("last_name")),
		Email:          strings.TrimSpace(r.PostForm.Get("email")),
		EmployeeNumber: strings.TrimSpace(r.PostForm.Get("employee_number")),
	})
	if err != nil {
		s.log.Warn("user update failed", "err", err)
		http.Error(w, friendlyAccessError(err), accessErrorStatus(err))
		return
	}
	http.Redirect(w, r, "/a/users/"+id, http.StatusSeeOther)
}

// handleAdminUsersActivate / Deactivate set the user status.
func (s *Server) handleAdminUsersActivate(w http.ResponseWriter, r *http.Request) {
	s.setUserStatus(w, r, access.StatusActive)
}

func (s *Server) handleAdminUsersDeactivate(w http.ResponseWriter, r *http.Request) {
	s.setUserStatus(w, r, access.StatusDeactivated)
}

func (s *Server) setUserStatus(w http.ResponseWriter, r *http.Request, status access.Status) {
	if !s.uaAvailable(r.Context()) {
		http.Error(w, "UA ist deaktiviert oder nicht konfiguriert.", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	if err := s.userStore.SetStatus(r.Context(), id, status); err != nil {
		s.log.Warn("user status failed", "err", err, "status", status)
		http.Error(w, friendlyAccessError(err), accessErrorStatus(err))
		return
	}
	target := "/a/users/" + id
	if r.Referer() != "" && strings.HasSuffix(r.Referer(), "/a/users") {
		target = "/a/users"
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleAdminUsersDelete deletes a user in UA.
func (s *Server) handleAdminUsersDelete(w http.ResponseWriter, r *http.Request) {
	if !s.uaAvailable(r.Context()) {
		http.Error(w, "UA ist deaktiviert oder nicht konfiguriert.", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	if err := s.userStore.Delete(r.Context(), id); err != nil {
		s.log.Warn("user delete failed", "err", err)
		http.Error(w, friendlyAccessError(err), accessErrorStatus(err))
		return
	}
	if r.Method == http.MethodDelete || wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"deleted": id})
		return
	}
	http.Redirect(w, r, "/a/users", http.StatusSeeOther)
}

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
