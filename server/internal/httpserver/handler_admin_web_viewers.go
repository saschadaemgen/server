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

	"github.com/skip2/go-qrcode"

	"unifix.local/server/internal/auth/argon2id"
	"unifix.local/server/internal/auth/loginaudit"
	"unifix.local/server/internal/mockmanager"
	"unifix.local/server/internal/password"
	"unifix.local/server/internal/platformconfig"
)

// macFormat matches the lowercase colon form, e.g. 0c:ea:14:42:42:42.
var macFormat = regexp.MustCompile(`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`)

// usernameFormat passt auf die Form, die sanitizeUsername liefert:
// 3-32 Zeichen aus lowercase a-z, 0-9, _ . und -. Wird als
// Sanity-Check nach dem Sanitize-Pass eingesetzt, nicht als
// Ersatz fuer den Sanitizer.
var usernameFormat = regexp.MustCompile(`^[a-z0-9._-]{3,32}$`)

// servicePortStart is the first port the auto-allocator hands out.
const servicePortStart = 8100

// adminWebViewersData ist die Payload fuer das templates/admin/web-viewers.html
// Template (eigene Implementierung, kein Library-Snippet).
type adminWebViewersData struct {
	User    adminUser
	Viewers []webViewerRow
	Flash   string
	FlashType string
}

type webViewerRow struct {
	MAC           string
	Name          string
	Username      string
	HasPassword   bool
	PasswordSetAt string
	LockedNow     bool
	OnlineNow     bool
}

func (s *Server) handleAdminWebViewersList(w http.ResponseWriter, r *http.Request) {
	data, err := s.buildWebViewersData(r)
	if err != nil {
		s.log.Error("list viewers", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.renderAdminPage(w, "web-viewers", data)
}

func (s *Server) buildWebViewersData(r *http.Request) (adminWebViewersData, error) {
	infos, err := s.mockMgr.ListViewers(r.Context())
	if err != nil {
		return adminWebViewersData{}, err
	}
	locked := map[string]bool{}
	if s.viewerLimiter != nil {
		for _, l := range s.viewerLimiter.LockedUsers() {
			locked[l.Value] = true
		}
	}
	username := AdminUserFromContext(r.Context())
	data := adminWebViewersData{
		User: adminUser{Name: username, Initials: initialsOf(username)},
	}
	for _, info := range infos {
		if info.Type != mockmanager.TypeWeb {
			continue
		}
		row := webViewerRow{
			MAC:         info.MAC,
			Name:        info.Name,
			Username:    info.Username,
			HasPassword: info.HasPassword,
			OnlineNow:   info.Running,
			LockedNow:   locked[info.Username],
		}
		if info.PasswordSetAt != nil {
			row.PasswordSetAt = info.PasswordSetAt.Format("02.01.2006 15:04")
		}
		data.Viewers = append(data.Viewers, row)
	}
	return data, nil
}

// credentialsResponse ist die JSON-Antwort nach Anlegen / Reset.
type credentialsResponse struct {
	MAC       string `json:"mac"`
	Name      string `json:"name"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	LoginURL  string `json:"login_url"`
	QRSVG     string `json:"qr_svg"`
}

// handleAdminWebViewersCreate legt einen neuen Web-Viewer an,
// generiert ein Passwort, persistiert Argon2id-Hash und gibt
// (URL+Username+Passwort+QR-SVG) als JSON oder HTML-Fragment
// zurueck (Accept-driven).
func (s *Server) handleAdminWebViewersCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "ungueltige Formular-Daten", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("name"))
	mac := strings.ToLower(strings.TrimSpace(r.PostForm.Get("mac")))
	suggestedUsername := strings.TrimSpace(r.PostForm.Get("username"))

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
	// Saison 13-02-FIX4-a-HOTFIX3: Username wird IMMER durch
	// sanitizeUsername gefiltert (Anlegen-Pfad spiegelt damit den
	// Login-Pfad). Wenn der Admin schon einen sauberen Username
	// eingegeben hat, ist die Operation idempotent.
	if suggestedUsername == "" {
		suggestedUsername = sanitizeUsername(name)
	} else {
		suggestedUsername = sanitizeUsername(suggestedUsername)
	}
	if !usernameFormat.MatchString(suggestedUsername) {
		http.Error(w,
			"Benutzername muss nach Normalisierung 3-32 Zeichen aus a-z, 0-9, _ und . haben.",
			http.StatusBadRequest)
		return
	}

	port, err := s.nextFreeServicePort(r.Context())
	if err != nil {
		s.log.Error("next free port", "err", err)
		http.Error(w, "Port-Allokation fehlgeschlagen.", http.StatusInternalServerError)
		return
	}

	spec := mockmanager.ViewerSpec{
		MAC:         mac,
		Name:        name,
		ServicePort: port,
		Type:        mockmanager.TypeWeb,
		Username:    suggestedUsername,
	}
	if err := s.mockMgr.AddViewer(r.Context(), spec); err != nil {
		switch {
		case errors.Is(err, mockmanager.ErrMACInUse):
			http.Error(w, "MAC bereits vergeben.", http.StatusConflict)
		case errors.Is(err, mockmanager.ErrPortInUse):
			http.Error(w, "Service-Port bereits vergeben.", http.StatusConflict)
		case errors.Is(err, mockmanager.ErrUsernameInUse):
			http.Error(w, "Benutzername bereits vergeben.", http.StatusConflict)
		default:
			s.log.Error("add viewer", "err", err, "mac_prefix", mac[:8])
			http.Error(w, "Anlegen fehlgeschlagen.", http.StatusInternalServerError)
		}
		return
	}

	resp, err := s.generateAndStorePassword(r.Context(), spec.MAC, spec.Name, suggestedUsername, r)
	if err != nil {
		s.log.Error("set initial password", "err", err)
		http.Error(w, "Passwort-Generierung fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	s.respondCredentials(w, r, resp)
}

// handleAdminWebViewersResetPW erzeugt ein neues Passwort und
// invalidiert alle alten Sessions des Viewers.
func (s *Server) handleAdminWebViewersResetPW(w http.ResponseWriter, r *http.Request) {
	mac := strings.ToLower(r.PathValue("mac"))
	if !macFormat.MatchString(mac) {
		http.Error(w, "ungueltige MAC", http.StatusBadRequest)
		return
	}
	info, err := s.mockMgr.GetViewerInfo(r.Context(), mac)
	if err != nil {
		if errors.Is(err, mockmanager.ErrViewerNotFound) {
			http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
			return
		}
		s.log.Error("get viewer info", "err", err, "mac_prefix", mac[:8])
		http.Error(w, "Lookup fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	resp, err := s.generateAndStorePassword(r.Context(), info.MAC, info.Name, info.Username, r)
	if err != nil {
		s.log.Error("reset password", "err", err)
		http.Error(w, "Reset fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	if _, err := s.sessions.RevokeAllForViewer(r.Context(), info.MAC); err != nil {
		s.log.Warn("revoke sessions after pw reset", "err", err)
	}
	s.respondCredentials(w, r, resp)
}

// handleAdminWebViewersUnlock hebt die Rate-Limit-Sperre fuer den
// Viewer-Username auf.
func (s *Server) handleAdminWebViewersUnlock(w http.ResponseWriter, r *http.Request) {
	mac := strings.ToLower(r.PathValue("mac"))
	if !macFormat.MatchString(mac) {
		http.Error(w, "ungueltige MAC", http.StatusBadRequest)
		return
	}
	info, err := s.mockMgr.GetViewerInfo(r.Context(), mac)
	if err != nil {
		http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
		return
	}
	if s.viewerLimiter != nil && info.Username != "" {
		s.viewerLimiter.ClearUser(info.Username)
	}
	s.recordAudit(r, loginaudit.Entry{
		Realm:     loginaudit.RealmViewer,
		Username:  info.Username,
		ViewerMAC: info.MAC,
		IP:        clientIP(r),
		UserAgent: r.UserAgent(),
		Outcome:   loginaudit.OutcomeUnlocked,
	})
	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"unlocked": true})
		return
	}
	http.Redirect(w, r, "/a/web-viewers", http.StatusSeeOther)
}

// handleAdminWebViewersRename benennt den Viewer um.
func (s *Server) handleAdminWebViewersRename(w http.ResponseWriter, r *http.Request) {
	mac := strings.ToLower(r.PathValue("mac"))
	if !macFormat.MatchString(mac) {
		http.Error(w, "ungueltige MAC", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "ungueltige Formular-Daten", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("name"))
	if name == "" || len(name) > 64 {
		http.Error(w, "Name fehlt oder zu lang.", http.StatusBadRequest)
		return
	}
	if err := s.mockMgr.Rename(r.Context(), mac, name); err != nil {
		if errors.Is(err, mockmanager.ErrViewerNotFound) {
			http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
			return
		}
		s.log.Error("rename viewer", "err", err, "mac_prefix", mac[:8])
		http.Error(w, "Rename fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/a/web-viewers", http.StatusSeeOther)
}

// handleAdminWebViewersDelete loescht einen Viewer (Sessions
// kaskadieren via FK).
func (s *Server) handleAdminWebViewersDelete(w http.ResponseWriter, r *http.Request) {
	mac := strings.ToLower(r.PathValue("mac"))
	if !macFormat.MatchString(mac) {
		http.Error(w, "ungueltige MAC", http.StatusBadRequest)
		return
	}
	if err := s.mockMgr.RemoveViewer(r.Context(), mac); err != nil {
		if errors.Is(err, mockmanager.ErrViewerNotFound) {
			http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
			return
		}
		s.log.Error("remove viewer", "err", err, "mac_prefix", mac[:8])
		http.Error(w, "Loeschen fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/a/web-viewers", http.StatusSeeOther)
}

// generateAndStorePassword erzeugt ein neues 16-Zeichen-Passwort,
// hasht es mit Argon2id+Pepper, schreibt den Hash und gibt
// Klartext-Passwort + QR-Code zurueck.
func (s *Server) generateAndStorePassword(ctx context.Context, mac, name, username string, r *http.Request) (credentialsResponse, error) {
	pw, err := password.Generate()
	if err != nil {
		return credentialsResponse{}, fmt.Errorf("password generate: %w", err)
	}
	pepper, _ := s.platformCfg.GetSecret(ctx, platformconfig.KeyViewerPwPepper)
	hash, err := argon2id.HashWithPepper(pw, pepper)
	if err != nil {
		return credentialsResponse{}, fmt.Errorf("argon2id hash: %w", err)
	}
	if err := s.mockMgr.SetPasswordHash(ctx, mac, hash); err != nil {
		return credentialsResponse{}, fmt.Errorf("store hash: %w", err)
	}
	loginURL := s.buildLoginURL(r, username, pw)
	qrSVG, err := renderQRSVG(loginURL, 240)
	if err != nil {
		s.log.Warn("qr render failed", "err", err)
		qrSVG = ""
	}
	return credentialsResponse{
		MAC:      mac,
		Name:     name,
		Username: username,
		Password: pw,
		LoginURL: loginURL,
		QRSVG:    qrSVG,
	}, nil
}

// buildLoginURL liefert nur die nackte Login-URL fuer den QR-Code.
//
// Saison 13-02-FIX4-a-HOTFIX1: Pre-Fill via ?u= und ?p= ist raus
// (Sicherheits-Anti-Pattern; Passwoerter im Query-String landen
// in Server-Logs, Browser-History und Referer-Headern). Mieter
// scannt den QR, kommt zur Login-Seite und tippt Username plus
// Passwort vom Zettel ab.
//
// Saison 13-02-FIX4-a-HOTFIX2: URL haengt jetzt auf /einloggen
// (mieterfreundlich, kein /m-Tech-Kuerzel mehr).
func (s *Server) buildLoginURL(r *http.Request, _ string, _ string) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/einloggen", scheme, r.Host)
}

func (s *Server) respondCredentials(w http.ResponseWriter, r *http.Request, c credentialsResponse) {
	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(c)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tpl.renderPartial(w, "credentials-modal", c)
}

func (s *Server) nextFreeServicePort(ctx context.Context) (uint16, error) {
	var maxPort sqlNullInt
	err := s.platformCfg.DB().QueryRowContext(ctx,
		`SELECT COALESCE(MAX(service_port), 0) FROM viewers`,
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

// wantsJSON peeks at the Accept header to decide whether to
// answer with JSON or HTML.
func wantsJSON(r *http.Request) bool {
	a := r.Header.Get("Accept")
	return strings.Contains(a, "application/json")
}

// renderQRSVG builds an inline SVG QR-Code for the given content.
func renderQRSVG(content string, size int) (string, error) {
	q, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return "", err
	}
	bits := q.Bitmap()
	n := len(bits)
	if n == 0 {
		return "", errors.New("qr: empty bitmap")
	}
	var b strings.Builder
	fmt.Fprintf(&b,
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" shape-rendering="crispEdges" role="img" aria-label="Login-QR-Code">`,
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



