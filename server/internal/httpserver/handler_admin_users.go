package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"unifix.local/server/internal/access"
)

const (
	usersDefaultPageSize = 20
	usersMaxPageSize     = 100
)

// adminUsersData ist die Payload fuer templates/admin/users.html.
type adminUsersData struct {
	User         adminUser
	Configured   bool
	Users        []userRow
	Total        int
	Page         int
	PageSize     int
	PageCount    int
	Prev         int
	Next         int
	Query        string
	StatusFilter string
	Flash        string
	FlashType    string
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

// adminUserDetailData fuer /a/users/{id}.
type adminUserDetailData struct {
	User           adminUser
	Configured     bool
	Profile        userRow
	LinkedViewers  []linkedViewerRow
	NotFoundFlash  string
	Flash          string
	FlashType      string
}

type linkedViewerRow struct {
	MAC      string
	Name     string
	Online   bool
	Username string
}

// handleAdminUsersList rendert /a/users mit Pagination + Suche +
// Status-Filter. Wenn UA-Token noch nicht konfiguriert: zeigt
// Hinweis-Karte statt leerer Tabelle.
func (s *Server) handleAdminUsersList(w http.ResponseWriter, r *http.Request) {
	username := AdminUserFromContext(r.Context())
	data := adminUsersData{
		User:       adminUser{Name: username, Initials: initialsOf(username)},
		Configured: s.userStore != nil && s.userStore.IsConfigured(),
		Page:       1,
		PageSize:   usersDefaultPageSize,
	}

	if !data.Configured {
		s.renderAdminPage(w, "users", data)
		return
	}

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
		s.renderAdminPage(w, "users", data)
		return
	}
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
	s.renderAdminPage(w, "users", data)
}

// handleAdminUsersListJSON liefert die gleiche Liste als JSON.
// Fuer AJAX-Pagination und potenziellen Dropdown im Web-Viewer-
// Anlege-Modal.
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

// handleAdminUsersDetail rendert /a/users/{id}.
func (s *Server) handleAdminUsersDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	username := AdminUserFromContext(r.Context())
	data := adminUserDetailData{
		User:       adminUser{Name: username, Initials: initialsOf(username)},
		Configured: s.userStore != nil && s.userStore.IsConfigured(),
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

// handleAdminUsersCreate verarbeitet POST /a/users (Anlegen via
// Modal-Form oder direkter POST).
func (s *Server) handleAdminUsersCreate(w http.ResponseWriter, r *http.Request) {
	if s.userStore == nil || !s.userStore.IsConfigured() {
		http.Error(w, "UA-API nicht konfiguriert.", http.StatusServiceUnavailable)
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

// handleAdminUsersUpdate verarbeitet POST /a/users/{id}/update.
func (s *Server) handleAdminUsersUpdate(w http.ResponseWriter, r *http.Request) {
	if s.userStore == nil || !s.userStore.IsConfigured() {
		http.Error(w, "UA-API nicht konfiguriert.", http.StatusServiceUnavailable)
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

// handleAdminUsersActivate / Deactivate setzen den Status.
func (s *Server) handleAdminUsersActivate(w http.ResponseWriter, r *http.Request) {
	s.setUserStatus(w, r, access.StatusActive)
}

func (s *Server) handleAdminUsersDeactivate(w http.ResponseWriter, r *http.Request) {
	s.setUserStatus(w, r, access.StatusDeactivated)
}

func (s *Server) setUserStatus(w http.ResponseWriter, r *http.Request, status access.Status) {
	if s.userStore == nil || !s.userStore.IsConfigured() {
		http.Error(w, "UA-API nicht konfiguriert.", http.StatusServiceUnavailable)
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

// handleAdminUsersDelete loescht einen User in UA.
func (s *Server) handleAdminUsersDelete(w http.ResponseWriter, r *http.Request) {
	if s.userStore == nil || !s.userStore.IsConfigured() {
		http.Error(w, "UA-API nicht konfiguriert.", http.StatusServiceUnavailable)
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

// parseListParams liest page/size/q/status aus der URL.
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
		// OK, behalten
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

// collectLinkedViewers laedt die Web-Viewer die per
// linked_ua_user_id auf diesen UA-User zeigen.
func (s *Server) collectLinkedViewers(r *http.Request, userID string) []linkedViewerRow {
	if s.mockMgr == nil || userID == "" {
		return nil
	}
	all, err := s.mockMgr.ListViewers(r.Context())
	if err != nil {
		s.log.Warn("linked viewers list failed", "err", err)
		return nil
	}
	out := make([]linkedViewerRow, 0)
	for _, v := range all {
		if v.LinkedUAUserID == userID {
			out = append(out, linkedViewerRow{
				MAC:      v.MAC,
				Name:     v.Name,
				Online:   v.Running,
				Username: v.Username,
			})
		}
	}
	return out
}
