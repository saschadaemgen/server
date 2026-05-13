package httpserver

import (
	"errors"
	"net/http"
	"strings"

	"unifix.local/server/internal/uaapi"
)

// adminUsersData is the payload for admin-users.html (Claude-
// Design library). Tenants are surfaced from our mock_viewers
// table plus the UA-Developer-API where available; the library
// expects a flat shape with magic-link state per row.
type adminUsersData struct {
	User       adminUser
	Tenants    []adminTenantRow
	Pagination adminPagination
}

type adminTenantRow struct {
	ID              string
	UnitName        string
	UnitMark        string
	TenantName      string
	Email           string
	MagicLinkStatus string // "active" | "expired" | "not-sent"
	MagicLinkExpiry string
	LastSeen        string
}

type adminPagination struct {
	Page    int
	Total   int
	PerPage int
}

// handleAdminUsersList renders the Mieter-Tabelle. We use the
// mock_viewers list as the row source since one mock-viewer is
// effectively one tenant device; UA-User data joins in where the
// developer API is reachable.
func (s *Server) handleAdminUsersList(w http.ResponseWriter, r *http.Request) {
	username := AdminUserFromContext(r.Context())
	data := adminUsersData{
		User: adminUser{Name: username, Initials: initialsOf(username)},
	}

	mocks, err := s.mockMgr.ListViewers(r.Context())
	if err != nil {
		s.log.Error("list viewers", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Pull UA users so we can join an email per row when possible.
	uaByName := map[string]uaapi.User{}
	if s.ua != nil {
		if users, err := s.ua.ListUsers(r.Context()); err == nil {
			for _, u := range users {
				uaByName[strings.ToLower(u.FirstName+" "+u.LastName)] = u
			}
		}
	}

	for _, m := range mocks {
		row := adminTenantRow{
			ID:              m.MAC,
			UnitName:        m.Name,
			UnitMark:        initialsOf(m.Name),
			TenantName:      m.Name,
			MagicLinkStatus: "not-sent",
		}
		if u, ok := uaByName[strings.ToLower(m.Name)]; ok {
			row.Email = u.UserEmail
			row.TenantName = u.FirstName + " " + u.LastName
		}
		if m.Running {
			row.LastSeen = "online"
		} else {
			row.LastSeen = "offline"
		}
		data.Tenants = append(data.Tenants, row)
	}

	data.Pagination = adminPagination{
		Page:    1,
		Total:   len(data.Tenants),
		PerPage: len(data.Tenants),
	}

	s.renderAdminPage(w, "users", data)
}

// handleAdminUsersCreate creates a UA-Developer-API user. The
// library form posts first_name + last_name + email; we keep
// the existing behaviour.
func (s *Server) handleAdminUsersCreate(w http.ResponseWriter, r *http.Request) {
	if s.ua == nil {
		http.Error(w, "UA-API nicht konfiguriert", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Ungueltige Formular-Daten.", http.StatusBadRequest)
		return
	}
	first := strings.TrimSpace(r.PostForm.Get("first_name"))
	last := strings.TrimSpace(r.PostForm.Get("last_name"))
	email := strings.TrimSpace(r.PostForm.Get("email"))
	if first == "" || last == "" {
		http.Error(w, "Vor- und Nachname sind Pflicht.", http.StatusBadRequest)
		return
	}
	_, err := s.ua.CreateUser(r.Context(), uaapi.User{
		FirstName: first, LastName: last, UserEmail: email,
	})
	if err != nil {
		if errors.Is(err, uaapi.ErrUnauthorized) {
			http.Error(w, "UA-API Token ungueltig.", http.StatusUnauthorized)
			return
		}
		s.log.Error("create ua user", "err", err)
		http.Error(w, "Anlegen fehlgeschlagen.", http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, "/a/users", http.StatusSeeOther)
}

// handleAdminUsersDelete deletes a UA-Developer-API user.
func (s *Server) handleAdminUsersDelete(w http.ResponseWriter, r *http.Request) {
	if s.ua == nil {
		http.Error(w, "UA-API nicht konfiguriert", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "id missing", http.StatusBadRequest)
		return
	}
	if err := s.ua.DeleteUser(r.Context(), id); err != nil {
		if errors.Is(err, uaapi.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if errors.Is(err, uaapi.ErrUnauthorized) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		s.log.Error("delete ua user", "err", err)
		http.Error(w, "delete failed", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
}
