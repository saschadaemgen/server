package httpserver

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"unifix.local/server/internal/mockmanager"
)

// macFormat matches the lowercase colon form, e.g. 0c:ea:14:42:42:42.
var macFormat = regexp.MustCompile(`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`)

// servicePortStart is the first port the auto-allocator hands out.
// Saison 12 default: 8100. Far enough from 8080 (UA-standard) and
// 8443 (server TLS) to avoid surprises.
const servicePortStart = 8100

type adminMocksData struct {
	Title     string
	ShowNav   bool
	ActiveNav string
	Mocks     []mockRowData
}

// mockRowData is the per-row payload for templates/admin/mocks_row.html.
// Saison 12-06: ua_user_id is gone; routing is mock-MAC only.
type mockRowData struct {
	MAC         string
	Name        string
	ServicePort uint16
	Running     bool
}

func (s *Server) handleAdminMocksList(w http.ResponseWriter, r *http.Request) {
	infos, err := s.mockMgr.ListViewers(r.Context())
	if err != nil {
		s.log.Error("list viewers", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := adminMocksData{
		Title:     "Mock-Viewer",
		ShowNav:   true,
		ActiveNav: "mocks",
		Mocks:     make([]mockRowData, 0, len(infos)),
	}
	for _, info := range infos {
		data.Mocks = append(data.Mocks, mockRowData{
			MAC:         info.MAC,
			Name:        info.Name,
			ServicePort: info.ServicePort,
			Running:     info.Running,
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
	w.WriteHeader(http.StatusOK)
}

// handleAdminMocksMagicLink generates a 24h magic-link bound to
// this mock and returns a clipboard-ready modal HTML fragment.
// Saison 12-06: no longer requires a tenant binding; any
// existing mock can issue a link.
func (s *Server) handleAdminMocksMagicLink(w http.ResponseWriter, r *http.Request) {
	mac := strings.ToLower(r.PathValue("mac"))
	if !macFormat.MatchString(mac) {
		s.respondHTMXError(w, http.StatusBadRequest, "Ungueltige MAC-Adresse.")
		return
	}
	info, err := s.mockMgr.GetViewerInfo(r.Context(), mac)
	if err != nil {
		if errors.Is(err, mockmanager.ErrViewerNotFound) {
			s.respondHTMXError(w, http.StatusNotFound, "Mock-Viewer nicht gefunden.")
			return
		}
		s.log.Error("get viewer info", "err", err, "mac_prefix", mac[:8])
		s.respondHTMXError(w, http.StatusInternalServerError, "Mock-Lookup fehlgeschlagen.")
		return
	}

	token, err := s.magic.CreateWithTTL(r.Context(), info.MAC, 24*time.Hour)
	if err != nil {
		s.log.Error("magic-link generate failed", "err", err, "mac_prefix", mac[:8])
		s.respondHTMXError(w, http.StatusInternalServerError, "Token-Erzeugung fehlgeschlagen.")
		return
	}

	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	loginURL := fmt.Sprintf("%s://%s/m/login?t=%s", scheme, r.Host, token)

	s.renderAdminPartial(w, "magic_link_modal", struct {
		URL      string
		MockMAC  string
		MockName string
	}{
		URL:      loginURL,
		MockMAC:  info.MAC,
		MockName: info.Name,
	})
}

func (s *Server) respondHTMXError(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	_ = s.tpl.renderPartial(w, "error", map[string]string{"Message": msg})
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
	var maxPort sqlNullInt
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

// sqlNullInt scans COALESCE results without dragging database/sql
// into every caller.
type sqlNullInt struct {
	Int int64
}

func (n *sqlNullInt) Scan(src any) error {
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
