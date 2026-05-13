package httpserver

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"

	"unifix.local/server/internal/mockmanager"
)

// macFormat matches the lowercase colon form, e.g. 0c:ea:14:42:42:42.
var macFormat = regexp.MustCompile(`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`)

// servicePortStart is the first port the auto-allocator hands out.
const servicePortStart = 8100

// adminMocksData carries the data the Claude-Design admin-mocks
// snippet expects: User, Units (mock viewers), Doors and the
// RecentMocks event log.
type adminMocksData struct {
	User        adminUser
	Units       []adminUnit
	Doors       []adminDoor
	RecentMocks []adminMockEvent
	CSRFToken   string
}

type adminUnit struct {
	ID         string
	Name       string
	TenantName string
	OnlineNow  bool
}

type adminDoor struct {
	ID   string
	Name string
}

type adminMockEvent struct {
	When     string
	UnitName string
	DoorName string
	Trigger  string
	Result   string // "delivered" | "offline" | "ignored" | other
}

func (s *Server) handleAdminMocksList(w http.ResponseWriter, r *http.Request) {
	infos, err := s.mockMgr.ListViewers(r.Context())
	if err != nil {
		s.log.Error("list viewers", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	username := AdminUserFromContext(r.Context())
	data := adminMocksData{
		User: adminUser{Name: username, Initials: initialsOf(username)},
		Doors: []adminDoor{
			{ID: "hauseingang", Name: "Hauseingang"},
		},
	}
	for _, info := range infos {
		data.Units = append(data.Units, adminUnit{
			ID:         info.MAC,
			Name:       info.Name,
			TenantName: info.Name,
			OnlineNow:  info.Running,
		})
	}

	// Recent mock-events: drain the most recent doorbell entries
	// across all mocks (best-effort: take the first mock's history
	// for now; a true union-query is a S13-03 follow-up).
	if s.history != nil && len(infos) > 0 {
		events, _ := s.history.ListForMock(r.Context(), infos[0].MAC, 10)
		for _, ev := range events {
			data.RecentMocks = append(data.RecentMocks, adminMockEvent{
				When:     ev.OccurredAt.Format("15:04"),
				UnitName: infos[0].Name,
				DoorName: doorNameFromIntercom(ev.IntercomMAC),
				Trigger:  "manuell",
				Result:   mockResultFor(ev.CancelledAt != nil, ev.ReadAt != nil),
			})
		}
	}

	s.renderAdminPage(w, "mocks", data)
}

func mockResultFor(cancelled, read bool) string {
	switch {
	case cancelled:
		return "ignored"
	case read:
		return "delivered"
	default:
		return "delivered"
	}
}

// handleAdminMocksCreate is unchanged from S13-01: form-POST a
// new mock viewer. The library admin-mocks page no longer ships
// a CRUD form, but the handler stays in place so future flows
// (e.g. an /a/units/create page) can re-use it.
func (s *Server) handleAdminMocksCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Ungueltige Formular-Daten.", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("name"))
	mac := strings.ToLower(strings.TrimSpace(r.PostForm.Get("mac")))

	if name == "" || len(name) > 64 {
		http.Error(w, "Name fehlt oder zu lang (max 64 Zeichen).", http.StatusBadRequest)
		return
	}
	if mac == "" {
		var err error
		mac, err = generateUbiquitiMAC()
		if err != nil {
			s.log.Error("generate mac", "err", err)
			http.Error(w, "MAC-Generierung fehlgeschlagen.", http.StatusInternalServerError)
			return
		}
	}
	if !macFormat.MatchString(mac) {
		http.Error(w,
			"MAC muss im Format xx:xx:xx:xx:xx:xx (lowercase hex) sein.",
			http.StatusBadRequest)
		return
	}

	port, err := s.nextFreeServicePort(r.Context())
	if err != nil {
		s.log.Error("next free port", "err", err)
		http.Error(w, "Port-Allokation fehlgeschlagen.", http.StatusInternalServerError)
		return
	}

	spec := mockmanager.ViewerSpec{MAC: mac, Name: name, ServicePort: port}
	if err := s.mockMgr.AddViewer(r.Context(), spec); err != nil {
		switch {
		case errors.Is(err, mockmanager.ErrMACInUse):
			http.Error(w, "MAC bereits vergeben.", http.StatusConflict)
		case errors.Is(err, mockmanager.ErrPortInUse):
			http.Error(w, "Service-Port bereits vergeben.", http.StatusConflict)
		default:
			s.log.Error("add viewer", "err", err, "mac_prefix", mac[:8])
			http.Error(w, "Anlegen fehlgeschlagen.", http.StatusInternalServerError)
		}
		return
	}

	http.Redirect(w, r, "/a/mocks", http.StatusSeeOther)
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

// magicLinkModalData is the payload for the Claude-Design
// modal-magic-link.html snippet. Library shape uses nested
// .Modal.{Unit, MagicLinkURL, ExpiresIn, QRSVG}.
type magicLinkModalData struct {
	Modal magicLinkModal
}

type magicLinkModal struct {
	Unit          magicLinkUnit
	MagicLinkURL  string
	ExpiresIn     string
	QRSVG         string
}

type magicLinkUnit struct {
	Name       string
	TenantName string
	Email      string
}

// magicLinkResponse is the JSON shape the admin-users glue
// script consumes after POST /a/users/{mac}/magic-link (and the
// legacy POST /a/mocks/{mac}/magic-link route). The qr_svg is
// an inline SVG ready to drop into the modal's .qr-box element.
type magicLinkResponse struct {
	URL            string `json:"url"`
	TenantName     string `json:"tenant_name"`
	ExpiresInHours int    `json:"expires_in_hours"`
	QRSVG          string `json:"qr_svg"`
}

// handleAdminMocksMagicLink generates a 24h magic-link bound to
// this mock and returns either the pre-rendered modal HTML
// fragment (Accept: text/html) or a JSON payload that the
// admin-users glue script can drop into the existing modal
// (Accept: application/json). The JSON path is the live-test-
// approved flow from S13-02-FIX3b; the HTML path stays for
// backwards compatibility with any test that still expects the
// modal markup directly.
func (s *Server) handleAdminMocksMagicLink(w http.ResponseWriter, r *http.Request) {
	mac := strings.ToLower(r.PathValue("mac"))
	if !macFormat.MatchString(mac) {
		http.Error(w, "Ungueltige MAC-Adresse.", http.StatusBadRequest)
		return
	}
	info, err := s.mockMgr.GetViewerInfo(r.Context(), mac)
	if err != nil {
		if errors.Is(err, mockmanager.ErrViewerNotFound) {
			http.Error(w, "Mock-Viewer nicht gefunden.", http.StatusNotFound)
			return
		}
		s.log.Error("get viewer info", "err", err, "mac_prefix", mac[:8])
		http.Error(w, "Mock-Lookup fehlgeschlagen.", http.StatusInternalServerError)
		return
	}

	token, err := s.magic.CreateWithTTL(r.Context(), info.MAC, 24*time.Hour)
	if err != nil {
		s.log.Error("magic-link generate failed", "err", err, "mac_prefix", mac[:8])
		http.Error(w, "Token-Erzeugung fehlgeschlagen.", http.StatusInternalServerError)
		return
	}

	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	loginURL := fmt.Sprintf("%s://%s/m/login?t=%s", scheme, r.Host, token)

	if wantsJSON(r) {
		qrSVG, err := renderQRSVG(loginURL, 240)
		if err != nil {
			s.log.Warn("qr render failed", "err", err)
			qrSVG = ""
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(magicLinkResponse{
			URL:            loginURL,
			TenantName:     info.Name,
			ExpiresInHours: 24,
			QRSVG:          qrSVG,
		})
		return
	}

	data := magicLinkModalData{Modal: magicLinkModal{
		Unit:         magicLinkUnit{Name: info.Name, TenantName: info.Name, Email: ""},
		MagicLinkURL: loginURL,
		ExpiresIn:    "in 24 Stunden",
	}}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.renderPartial(w, "modal-magic-link.html", data); err != nil {
		s.log.Error("render magic-link modal", "err", err)
		http.Error(w, "Modal-Render fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
}

// wantsJSON peeks at the Accept header to decide whether to
// answer with JSON (admin-users glue script) or HTML (legacy
// magic-link-modal-as-fragment path). We deliberately do NOT
// rely on the request method - both flavours come in as POST.
func wantsJSON(r *http.Request) bool {
	a := r.Header.Get("Accept")
	return strings.Contains(a, "application/json")
}

// renderQRSVG builds an inline SVG QR-Code for the given content.
// The bitmap is rendered as a single dark <path> for compactness;
// a 1-module quiet zone is added to keep scanners happy. Dimension
// "size" is the target pixel-width on screen.
func renderQRSVG(content string, size int) (string, error) {
	q, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return "", err
	}
	bits := q.Bitmap()
	n := len(bits) // total modules incl. 4-module quiet zone the lib already adds
	if n == 0 {
		return "", errors.New("qr: empty bitmap")
	}
	var b strings.Builder
	fmt.Fprintf(&b,
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" shape-rendering="crispEdges" role="img" aria-label="Magic-Link QR-Code">`,
		n, n, size, size)
	b.WriteString(`<rect width="100%" height="100%" fill="#ffffff"/>`)
	b.WriteString(`<path fill="#000000" d="`)
	for y, row := range bits {
		for x, on := range row {
			if !on {
				continue
			}
			fmt.Fprintf(&b, "M%d %dh1v1h-1z", x, y)
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String(), nil
}

// generateUbiquitiMAC produces 0c:ea:14:XX:XX:XX.
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
