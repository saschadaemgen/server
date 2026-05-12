package httpserver

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"unifix.local/server/internal/mockmanager"
	"unifix.local/server/internal/uaapi"
)

// macFormat matches the lowercase colon form, e.g. 0c:ea:14:42:42:42.
var macFormat = regexp.MustCompile(`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`)

// servicePortStart is the first port the auto-allocator hands out.
// Saison 12 default: 8100. Far enough from 8080 (UA-standard) and
// 8443 (server TLS) to avoid surprises.
const servicePortStart = 8100

type adminMocksData struct {
	Title   string
	ShowNav bool
	Mocks   []mockRowData
}

type mockRowData struct {
	MAC         string
	Name        string
	ServicePort uint16
	Running     bool
	UAUserID    string
	Users       []uaapi.User // for the binding dropdown
}

func (s *Server) handleAdminMocksList(w http.ResponseWriter, r *http.Request) {
	infos, err := s.mockMgr.ListViewers(r.Context())
	if err != nil {
		s.log.Error("list viewers", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	users := s.loadUsersForBinding(r.Context())

	data := adminMocksData{
		Title:   "Mock-Viewer",
		ShowNav: true,
		Mocks:   make([]mockRowData, 0, len(infos)),
	}
	for _, info := range infos {
		data.Mocks = append(data.Mocks, mockRowData{
			MAC:         info.MAC,
			Name:        info.Name,
			ServicePort: info.ServicePort,
			Running:     info.Running,
			UAUserID:    info.UAUserID,
			Users:       users,
		})
	}
	s.renderAdminPage(w, "mocks_list", data)
}

func (s *Server) handleAdminMocksCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.respondHTMXError(w, http.StatusBadRequest, "Ungueltige Formular-Daten.")
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("name"))
	mac := strings.ToLower(strings.TrimSpace(r.PostForm.Get("mac")))

	if name == "" || len(name) > 64 {
		s.respondHTMXError(w, http.StatusBadRequest, "Name fehlt oder zu lang (max 64 Zeichen).")
		return
	}
	if mac == "" {
		var err error
		mac, err = generateUbiquitiMAC()
		if err != nil {
			s.log.Error("generate mac", "err", err)
			s.respondHTMXError(w, http.StatusInternalServerError, "MAC-Generierung fehlgeschlagen.")
			return
		}
	}
	if !macFormat.MatchString(mac) {
		s.respondHTMXError(w, http.StatusBadRequest,
			"MAC muss im Format xx:xx:xx:xx:xx:xx (lowercase hex) sein.")
		return
	}

	port, err := s.nextFreeServicePort(r.Context())
	if err != nil {
		s.log.Error("next free port", "err", err)
		s.respondHTMXError(w, http.StatusInternalServerError, "Port-Allokation fehlgeschlagen.")
		return
	}

	spec := mockmanager.ViewerSpec{MAC: mac, Name: name, ServicePort: port}
	if err := s.mockMgr.AddViewer(r.Context(), spec); err != nil {
		switch {
		case errors.Is(err, mockmanager.ErrMACInUse):
			s.respondHTMXError(w, http.StatusConflict, "MAC bereits vergeben.")
		case errors.Is(err, mockmanager.ErrPortInUse):
			s.respondHTMXError(w, http.StatusConflict, "Service-Port bereits vergeben.")
		default:
			s.log.Error("add viewer", "err", err, "mac_prefix", mac[:8])
			s.respondHTMXError(w, http.StatusInternalServerError, "Anlegen fehlgeschlagen.")
		}
		return
	}

	row := mockRowData{
		MAC:         mac,
		Name:        name,
		ServicePort: port,
		Running:     true,
		Users:       s.loadUsersForBinding(r.Context()),
	}
	s.renderAdminPartial(w, "mock_row", row)
}

func (s *Server) handleAdminMocksDelete(w http.ResponseWriter, r *http.Request) {
	mac := strings.ToLower(r.PathValue("mac"))
	if !macFormat.MatchString(mac) {
		http.Error(w, "invalid mac", http.StatusBadRequest)
		return
	}
	if err := s.mockMgr.RemoveViewer(r.Context(), mac); err != nil {
		if errors.Is(err, mockmanager.ErrViewerNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		s.log.Error("remove viewer", "err", err, "mac_prefix", mac[:8])
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// htmx swaps outerHTML with an empty body, dropping the row.
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleAdminMocksBinding(w http.ResponseWriter, r *http.Request) {
	mac := strings.ToLower(r.PathValue("mac"))
	if !macFormat.MatchString(mac) {
		http.Error(w, "invalid mac", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	uaUserID := strings.TrimSpace(r.PostForm.Get("ua_user_id"))
	if err := s.mockMgr.UpdateUserBinding(r.Context(), mac, uaUserID); err != nil {
		if errors.Is(err, mockmanager.ErrViewerNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		s.log.Error("update binding", "err", err, "mac_prefix", mac[:8])
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// re-render the row so the dropdown reflects the new selection.
	infos, _ := s.mockMgr.ListViewers(r.Context())
	users := s.loadUsersForBinding(r.Context())
	for _, info := range infos {
		if info.MAC == mac {
			s.renderAdminPartial(w, "mock_row", mockRowData{
				MAC: info.MAC, Name: info.Name, ServicePort: info.ServicePort,
				Running: info.Running, UAUserID: info.UAUserID, Users: users,
			})
			return
		}
	}
	http.Error(w, "not found", http.StatusNotFound)
}

func (s *Server) respondHTMXError(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	_ = s.tpl.renderPartial(w, "error", map[string]string{"Message": msg})
}

// loadUsersForBinding returns the list shown in the dropdown of
// the mock-row. Returns nil silently if the UA client is not
// configured; the dropdown will just show "nicht zugeordnet".
func (s *Server) loadUsersForBinding(ctx context.Context) []uaapi.User {
	if s.ua == nil {
		return nil
	}
	users, err := s.ua.ListUsers(ctx)
	if err != nil {
		s.log.Warn("list ua users for binding dropdown failed", "err", err)
		return nil
	}
	return users
}

// generateUbiquitiMAC produces 0c:ea:14:XX:XX:XX with the
// trailing three bytes drawn from crypto/rand. The Ubiquiti OUI
// keeps the mock indistinguishable from real UA hardware in
// quick visual inspections.
func generateUbiquitiMAC() (string, error) {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("0c:ea:14:%02x:%02x:%02x", b[0], b[1], b[2]), nil
}

func (s *Server) nextFreeServicePort(ctx context.Context) (uint16, error) {
	var maxPort sql_NullInt
	err := s.platformCfg.DB().QueryRowContext(ctx,
		`SELECT COALESCE(MAX(service_port), 0) FROM mock_viewers`,
	).Scan(&maxPort.Int)
	if err != nil {
		return 0, err
	}
	if maxPort.Int < servicePortStart {
		return servicePortStart, nil
	}
	return uint16(maxPort.Int + 1), nil
}

// sql_NullInt is a tiny shim to scan COALESCE results without
// pulling in database/sql at the top-level scope.
type sql_NullInt struct {
	Int int64
}

func (n *sql_NullInt) Scan(src any) error {
	if src == nil {
		n.Int = 0
		return nil
	}
	switch v := src.(type) {
	case int64:
		n.Int = v
	case int:
		n.Int = int64(v)
	default:
		return fmt.Errorf("nextFreeServicePort: unexpected type %T", src)
	}
	return nil
}
