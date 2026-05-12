package httpserver

import (
	"errors"
	"net/http"
	"strings"

	"unifix.local/server/internal/uaapi"
)

type adminUsersData struct {
	Title         string
	ShowNav       bool
	NotConfigured bool
	Users         []uaapi.User
}

func (s *Server) handleAdminUsersList(w http.ResponseWriter, r *http.Request) {
	data := adminUsersData{Title: "Mieter", ShowNav: true}
	if s.ua == nil {
		data.NotConfigured = true
		s.renderAdminPage(w, "users_list", data)
		return
	}
	users, err := s.ua.ListUsers(r.Context())
	if err != nil {
		if errors.Is(err, uaapi.ErrUnauthorized) {
			data.NotConfigured = true
			s.renderAdminPage(w, "users_list", data)
			return
		}
		s.log.Error("list ua users", "err", err)
		http.Error(w, "ua api: "+err.Error(), http.StatusBadGateway)
		return
	}
	data.Users = users
	s.renderAdminPage(w, "users_list", data)
}

func (s *Server) handleAdminUsersCreate(w http.ResponseWriter, r *http.Request) {
	if s.ua == nil {
		s.respondHTMXError(w, http.StatusServiceUnavailable, "UA-API nicht konfiguriert.")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.respondHTMXError(w, http.StatusBadRequest, "Ungueltige Formular-Daten.")
		return
	}
	first := strings.TrimSpace(r.PostForm.Get("first_name"))
	last := strings.TrimSpace(r.PostForm.Get("last_name"))
	email := strings.TrimSpace(r.PostForm.Get("email"))
	if first == "" || last == "" {
		s.respondHTMXError(w, http.StatusBadRequest, "Vor- und Nachname sind Pflicht.")
		return
	}

	created, err := s.ua.CreateUser(r.Context(), uaapi.User{
		FirstName: first, LastName: last, Email: email,
	})
	if err != nil {
		if errors.Is(err, uaapi.ErrUnauthorized) {
			s.respondHTMXError(w, http.StatusUnauthorized,
				"UA-API Token ungueltig. Bitte unter Einstellungen pruefen.")
			return
		}
		s.log.Error("create ua user", "err", err)
		s.respondHTMXError(w, http.StatusBadGateway, "Anlegen fehlgeschlagen: "+err.Error())
		return
	}
	s.renderAdminPartial(w, "user_row", *created)
}

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
	// htmx outerHTML swap with empty body drops the row.
	w.WriteHeader(http.StatusOK)
}
