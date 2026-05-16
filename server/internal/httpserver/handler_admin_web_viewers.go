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

	"unifix.local/server/internal/access"
	"unifix.local/server/internal/auth/argon2id"
	"unifix.local/server/internal/auth/loginaudit"
	"unifix.local/server/internal/mockmanager"
	"unifix.local/server/internal/password"
	"unifix.local/server/internal/platformconfig"
)

// macFormat matches the lowercase colon form, e.g. 0c:ea:14:42:42:42.
var macFormat = regexp.MustCompile(`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`)

// servicePortStart is the first port the auto-allocator hands out.
const servicePortStart = 8100

// adminWebViewersData ist die Payload fuer das templates/admin/web-viewers.html
// Template (eigene Implementierung, kein Library-Snippet).
type adminWebViewersData struct {
	User      adminUser
	Viewers   []webViewerRow
	Flash     string
	FlashType string
}

type webViewerRow struct {
	MAC               string
	Name              string
	HasPassword       bool
	PasswordSetAt     string
	LockedNow         bool
	OnlineNow         bool
	LinkedUserID      string
	LinkedUserName    string
	PairedIntercomMAC string
	StreamProfile     string
	LoginURL          string
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
	adminUserName := AdminUserFromContext(r.Context())
	data := adminWebViewersData{
		User: adminUser{Name: adminUserName, Initials: initialsOf(adminUserName)},
	}
	// User-IDs sammeln und in einem Call beim UserStore aufloesen,
	// damit die Liste die Verknuepfungs-Spalte ohne N+1-Requests
	// rendern kann.
	userNames := map[string]string{}
	if s.userStore != nil && s.userStore.IsConfigured() {
		needed := map[string]bool{}
		for _, info := range infos {
			if info.LinkedUAUserID != "" {
				needed[info.LinkedUAUserID] = true
			}
		}
		if len(needed) > 0 {
			res, err := s.userStore.List(r.Context(), access.ListParams{Page: 1, Size: usersMaxPageSize})
			if err == nil {
				for _, u := range res.Users {
					if needed[u.ID] {
						userNames[u.ID] = u.DisplayName()
					}
				}
			}
		}
	}
	loginURL := s.buildLoginURL(r)
	for _, info := range infos {
		if info.Type != mockmanager.TypeWeb {
			continue
		}
		nameKey := mockmanager.NormalizeName(info.Name)
		row := webViewerRow{
			MAC:               info.MAC,
			Name:              info.Name,
			HasPassword:       info.HasPassword,
			OnlineNow:         info.Running,
			LockedNow:         locked[nameKey],
			LinkedUserID:      info.LinkedUAUserID,
			PairedIntercomMAC: info.PairedIntercomMAC,
			StreamProfile:     info.StreamProfile,
			LoginURL:          loginURL,
		}
		if info.PasswordSetAt != nil {
			row.PasswordSetAt = info.PasswordSetAt.Format("02.01.2006 15:04")
		}
		if linkedName, ok := userNames[info.LinkedUAUserID]; ok {
			row.LinkedUserName = linkedName
		}
		data.Viewers = append(data.Viewers, row)
	}
	return data, nil
}

// credentialsResponse ist die JSON-Antwort nach Anlegen / Reset.
type credentialsResponse struct {
	MAC      string `json:"mac"`
	Name     string `json:"name"`
	Password string `json:"password"`
	LoginURL string `json:"login_url"`
	QRSVG    string `json:"qr_svg"`
}

// handleAdminWebViewersCreate legt einen neuen Web-Viewer an.
//
// Saison 13-02-FIX4-a-HOTFIX4: radikal vereinfachte Maske. Der
// Wohnungs-Name ist der einzige Pflicht-Input, plus optional
// das Passwort (leer = Server generiert) und optional
// linked_ua_user_id. MAC wird intern erzeugt.
func (s *Server) handleAdminWebViewersCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "ungueltige Formular-Daten", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("name"))
	manualPW := r.PostForm.Get("password")
	linkedUser := strings.TrimSpace(r.PostForm.Get("linked_ua_user_id"))
	pairedIntercom := strings.ToLower(strings.TrimSpace(r.PostForm.Get("paired_intercom_mac")))
	streamProfile := strings.TrimSpace(r.PostForm.Get("stream_profile"))

	if name == "" || len(name) > 64 {
		http.Error(w, "Wohnungs-Name fehlt oder zu lang (max 64 Zeichen).", http.StatusBadRequest)
		return
	}
	if pairedIntercom != "" && !macFormat.MatchString(pairedIntercom) {
		http.Error(w, "Klingel-MAC muss lowercase xx:xx:xx:xx:xx:xx sein.", http.StatusBadRequest)
		return
	}

	mac, err := generateUbiquitiMAC()
	if err != nil {
		s.log.Error("generate mac", "err", err)
		http.Error(w, "MAC-Generierung fehlgeschlagen.", http.StatusInternalServerError)
		return
	}

	port, err := s.nextFreeServicePort(r.Context())
	if err != nil {
		s.log.Error("next free port", "err", err)
		http.Error(w, "Port-Allokation fehlgeschlagen.", http.StatusInternalServerError)
		return
	}

	spec := mockmanager.ViewerSpec{
		MAC:               mac,
		Name:              name,
		ServicePort:       port,
		Type:              mockmanager.TypeWeb,
		LinkedUAUserID:    linkedUser,
		PairedIntercomMAC: pairedIntercom,
		StreamProfile:     streamProfile,
	}
	if err := s.mockMgr.AddViewer(r.Context(), spec); err != nil {
		switch {
		case errors.Is(err, mockmanager.ErrMACInUse):
			http.Error(w, "MAC bereits vergeben (RNG-Kollision; bitte erneut anlegen).", http.StatusConflict)
		case errors.Is(err, mockmanager.ErrPortInUse):
			http.Error(w, "Service-Port bereits vergeben.", http.StatusConflict)
		case errors.Is(err, mockmanager.ErrNameInUse):
			http.Error(w,
				"Wohnungs-Name ist bereits vergeben (case-insensitive). Bitte einen anderen Namen waehlen.",
				http.StatusConflict)
		default:
			s.log.Error("add viewer", "err", err, "mac_prefix", mac[:8])
			http.Error(w, "Anlegen fehlgeschlagen.", http.StatusInternalServerError)
		}
		return
	}

	resp, err := s.storePasswordForViewer(r.Context(), spec.MAC, spec.Name, manualPW, r)
	if err != nil {
		s.log.Error("set initial password", "err", err)
		http.Error(w, "Passwort-Speicherung fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	s.respondCredentials(w, r, resp)
}

// handleAdminWebViewersResetPW erzeugt ein neues Zufalls-Passwort
// und invalidiert alle alten Sessions des Viewers. Fuer den
// "Passwort vergessen"-Pfad.
func (s *Server) handleAdminWebViewersResetPW(w http.ResponseWriter, r *http.Request) {
	s.changePassword(w, r, "")
}

// handleAdminWebViewersSetPassword setzt ein vom Admin manuell
// vorgegebenes Passwort. Body: { password: "..." }. Wirkung wie
// Reset-PW (Hash speichern, Sessions invalidieren), aber das
// Passwort ist vorgegeben.
//
// Saison 13-02-FIX4-a-HOTFIX4: keine Mindest-Anforderungen an
// das Passwort - das hier ist eine Klingel-Anlage, kein Online-
// Banking. Admin darf "1234" eingeben wenn er will.
func (s *Server) handleAdminWebViewersSetPassword(w http.ResponseWriter, r *http.Request) {
	pw := ""
	if r.Header.Get("Content-Type") == "application/json" {
		var body struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "ungueltiges JSON", http.StatusBadRequest)
			return
		}
		pw = body.Password
	} else {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "ungueltige Formular-Daten", http.StatusBadRequest)
			return
		}
		pw = r.PostForm.Get("password")
	}
	if pw == "" {
		http.Error(w, "Passwort darf nicht leer sein.", http.StatusBadRequest)
		return
	}
	s.changePassword(w, r, pw)
}

// changePassword ist der gemeinsame Code-Pfad fuer Reset-PW
// (manualPW="") und Set-Password (manualPW vorgegeben).
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, manualPW string) {
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
	resp, err := s.storePasswordForViewer(r.Context(), info.MAC, info.Name, manualPW, r)
	if err != nil {
		s.log.Error("change password", "err", err)
		http.Error(w, "Passwort-Speicherung fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	if _, err := s.sessions.RevokeAllForViewer(r.Context(), info.MAC); err != nil {
		s.log.Warn("revoke sessions after pw change", "err", err)
	}
	s.respondCredentials(w, r, resp)
}

// handleAdminWebViewersUnlock hebt die Rate-Limit-Sperre fuer den
// Viewer-Login auf.
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
	nameKey := mockmanager.NormalizeName(info.Name)
	if s.viewerLimiter != nil && nameKey != "" {
		s.viewerLimiter.ClearUser(nameKey)
	}
	s.recordAudit(r, loginaudit.Entry{
		Realm:     loginaudit.RealmViewer,
		Username:  info.Name,
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

// handleAdminWebViewersEdit ist der atomische Edit-Pfad fuer
// Name + Passwort + Verknuepfung in EINEM Request. Body
// (form-encoded oder JSON): name (Pflicht), password (leer =
// nicht aendern), linked_ua_user_id (leer = keine Verknuepfung).
//
// Saison 13-02-FIX4-a-HOTFIX5: ersetzt die drei einzelnen
// Endpoints rename / set-password / link aus dem Bearbeiten-
// Modal-Flow. Hintergrund: Single-Save-Klick mit klarer
// Erwartung welche Felder genau geaendert wurden.
func (s *Server) handleAdminWebViewersEdit(w http.ResponseWriter, r *http.Request) {
	mac := strings.ToLower(r.PathValue("mac"))
	if !macFormat.MatchString(mac) {
		http.Error(w, "ungueltige MAC", http.StatusBadRequest)
		return
	}

	var newName, newPW, newLink, newPaired, newStreamProfile string
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			Name              string `json:"name"`
			Password          string `json:"password"`
			LinkedUAUserID    string `json:"linked_ua_user_id"`
			PairedIntercomMAC string `json:"paired_intercom_mac"`
			StreamProfile     string `json:"stream_profile"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "ungueltiges JSON", http.StatusBadRequest)
			return
		}
		newName = strings.TrimSpace(body.Name)
		newPW = body.Password
		newLink = strings.TrimSpace(body.LinkedUAUserID)
		newPaired = strings.ToLower(strings.TrimSpace(body.PairedIntercomMAC))
		newStreamProfile = strings.TrimSpace(body.StreamProfile)
	} else {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "ungueltige Formular-Daten", http.StatusBadRequest)
			return
		}
		newName = strings.TrimSpace(r.PostForm.Get("name"))
		newPW = r.PostForm.Get("password")
		newLink = strings.TrimSpace(r.PostForm.Get("linked_ua_user_id"))
		newPaired = strings.ToLower(strings.TrimSpace(r.PostForm.Get("paired_intercom_mac")))
		newStreamProfile = strings.TrimSpace(r.PostForm.Get("stream_profile"))
	}

	if newName == "" || len(newName) > 64 {
		http.Error(w, "Wohnungs-Name fehlt oder zu lang (max 64 Zeichen).", http.StatusBadRequest)
		return
	}
	if newPaired != "" && !macFormat.MatchString(newPaired) {
		http.Error(w, "Klingel-MAC muss lowercase xx:xx:xx:xx:xx:xx sein.", http.StatusBadRequest)
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

	nameChanged := mockmanager.NormalizeName(info.Name) != mockmanager.NormalizeName(newName)
	pwChanged := newPW != ""
	linkChanged := info.LinkedUAUserID != newLink
	pairedChanged := info.PairedIntercomMAC != newPaired
	streamProfileChanged := info.StreamProfile != newStreamProfile

	if nameChanged {
		if err := s.mockMgr.Rename(r.Context(), mac, newName); err != nil {
			switch {
			case errors.Is(err, mockmanager.ErrNameInUse):
				http.Error(w,
					"Wohnungs-Name ist bereits vergeben (case-insensitive). Bitte einen anderen Namen waehlen.",
					http.StatusConflict)
			case errors.Is(err, mockmanager.ErrViewerNotFound):
				http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
			default:
				s.log.Error("edit rename", "err", err, "mac_prefix", mac[:8])
				http.Error(w, "Umbenennen fehlgeschlagen.", http.StatusInternalServerError)
			}
			return
		}
	}

	if linkChanged {
		if err := s.mockMgr.SetLinkedUAUserID(r.Context(), mac, newLink); err != nil {
			s.log.Error("edit set link", "err", err, "mac_prefix", mac[:8])
			http.Error(w, "Verknuepfung fehlgeschlagen.", http.StatusInternalServerError)
			return
		}
	}

	if pairedChanged {
		if err := s.mockMgr.SetPairedIntercomMAC(r.Context(), mac, newPaired); err != nil {
			s.log.Error("edit set paired intercom", "err", err, "mac_prefix", mac[:8])
			http.Error(w, "Klingel-Verknuepfung fehlgeschlagen.", http.StatusInternalServerError)
			return
		}
	}

	if streamProfileChanged {
		if err := s.mockMgr.SetStreamProfile(r.Context(), mac, newStreamProfile); err != nil {
			s.log.Error("edit set stream profile", "err", err, "mac_prefix", mac[:8])
			http.Error(w, "Stream-Profil-Speicherung fehlgeschlagen.", http.StatusInternalServerError)
			return
		}
	}

	// Passwort als letztes, damit ein Hash-Fehler die schon
	// gespeicherten Name- und Link-Aenderungen nicht zurueckwirft
	// (wir muessen sowieso roll-forward operieren, weil SQLite
	// ohne SAVEPOINT kein partielles Rollback unterstuetzt).
	var resp credentialsResponse
	if pwChanged {
		resp, err = s.storePasswordForViewer(r.Context(), info.MAC, newName, newPW, r)
		if err != nil {
			s.log.Error("edit set password", "err", err, "mac_prefix", mac[:8])
			http.Error(w, "Passwort-Speicherung fehlgeschlagen.", http.StatusInternalServerError)
			return
		}
		if _, err := s.sessions.RevokeAllForViewer(r.Context(), info.MAC); err != nil {
			s.log.Warn("revoke sessions after pw change", "err", err)
		}
	}

	s.log.Info("web viewer edited",
		"mac_prefix", mac[:8],
		"name_changed", nameChanged,
		"pw_changed", pwChanged,
		"link_changed", linkChanged,
		"paired_changed", pairedChanged,
		"stream_profile_changed", streamProfileChanged,
	)

	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		payload := map[string]any{
			"mac":                    info.MAC,
			"name":                   newName,
			"linked_ua_user_id":      newLink,
			"paired_intercom_mac":    newPaired,
			"stream_profile":         newStreamProfile,
			"name_changed":           nameChanged,
			"pw_changed":             pwChanged,
			"link_changed":           linkChanged,
			"paired_changed":         pairedChanged,
			"stream_profile_changed": streamProfileChanged,
		}
		if pwChanged {
			payload["password"] = resp.Password
			payload["login_url"] = resp.LoginURL
			payload["qr_svg"] = resp.QRSVG
		}
		_ = json.NewEncoder(w).Encode(payload)
		return
	}
	http.Redirect(w, r, "/a/web-viewers", http.StatusSeeOther)
}

// handleAdminWebViewersLoginInfo liefert den fertigen Mieter-
// Login-Link plus einen QR-Code-SVG fuer einen einzelnen
// Web-Viewer. Wird vom Edit-Modal-JS lazy beim Oeffnen abgerufen.
// Beide Felder kommen aus existierender Bauchemie:
// buildLoginURL() und renderQRSVG() (Saison 13-02-FIX4-a).
func (s *Server) handleAdminWebViewersLoginInfo(w http.ResponseWriter, r *http.Request) {
	mac := strings.ToLower(r.PathValue("mac"))
	if !macFormat.MatchString(mac) {
		http.Error(w, "ungueltige MAC", http.StatusBadRequest)
		return
	}
	if _, err := s.mockMgr.GetViewerInfo(r.Context(), mac); err != nil {
		if errors.Is(err, mockmanager.ErrViewerNotFound) {
			http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
			return
		}
		s.log.Error("login info get viewer", "err", err, "mac_prefix", mac[:8])
		http.Error(w, "Lookup fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	url := s.buildLoginURL(r)
	qrSVG, err := renderQRSVG(url, 240)
	if err != nil {
		s.log.Warn("qr render failed", "err", err)
		qrSVG = ""
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"login_url": url,
		"qr_svg":    qrSVG,
	})
}

// handleAdminWebViewersGeneratePW liefert ein frisches Zufalls-
// Passwort OHNE es zu speichern. Das UI uebernimmt es ins
// Passwort-Feld; gespeichert wird erst beim "Speichern"-Klick
// im Bearbeiten-Modal.
func (s *Server) handleAdminWebViewersGeneratePW(w http.ResponseWriter, r *http.Request) {
	pw, err := password.Generate()
	if err != nil {
		s.log.Error("generate password", "err", err)
		http.Error(w, "Passwort-Generierung fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]string{"password": pw})
}

// handleAdminWebViewersSetLink setzt oder loescht die
// Verknuepfung viewer.linked_ua_user_id. Empty user_id loescht.
func (s *Server) handleAdminWebViewersSetLink(w http.ResponseWriter, r *http.Request) {
	mac := strings.ToLower(r.PathValue("mac"))
	if !macFormat.MatchString(mac) {
		http.Error(w, "ungueltige MAC", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "ungueltige Formular-Daten.", http.StatusBadRequest)
		return
	}
	userID := strings.TrimSpace(r.PostForm.Get("linked_ua_user_id"))
	if err := s.mockMgr.SetLinkedUAUserID(r.Context(), mac, userID); err != nil {
		if errors.Is(err, mockmanager.ErrViewerNotFound) {
			http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
			return
		}
		s.log.Error("set linked user", "err", err, "mac_prefix", mac[:8])
		http.Error(w, "Verknuepfung fehlgeschlagen.", http.StatusInternalServerError)
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

// storePasswordForViewer hasht das Passwort mit Argon2id+Pepper,
// schreibt den Hash, und gibt das Klartext-Passwort plus QR-Code
// zurueck. Wenn manualPW leer ist, wird intern eines via
// password.Generate erzeugt; sonst nimmt der Caller-vorgegebene
// Wert direkt.
//
// Saison 13-02-FIX4-a-HOTFIX4: KEINE Mindest-Anforderungen an
// das vom Admin getippte Passwort. Klingel-Anlage, kein Online-
// Banking. Admin darf "1234" eingeben wenn er will.
func (s *Server) storePasswordForViewer(ctx context.Context, mac, name, manualPW string, r *http.Request) (credentialsResponse, error) {
	pw := manualPW
	if pw == "" {
		generated, err := password.Generate()
		if err != nil {
			return credentialsResponse{}, fmt.Errorf("password generate: %w", err)
		}
		pw = generated
	}
	pepper, _ := s.platformCfg.GetSecret(ctx, platformconfig.KeyViewerPwPepper)
	hash, err := argon2id.HashWithPepper(pw, pepper)
	if err != nil {
		return credentialsResponse{}, fmt.Errorf("argon2id hash: %w", err)
	}
	if err := s.mockMgr.SetPasswordHash(ctx, mac, hash); err != nil {
		return credentialsResponse{}, fmt.Errorf("store hash: %w", err)
	}
	loginURL := s.buildLoginURL(r)
	qrSVG, err := renderQRSVG(loginURL, 240)
	if err != nil {
		s.log.Warn("qr render failed", "err", err)
		qrSVG = ""
	}
	return credentialsResponse{
		MAC:      mac,
		Name:     name,
		Password: pw,
		LoginURL: loginURL,
		QRSVG:    qrSVG,
	}, nil
}

// buildLoginURL liefert nur die nackte Login-URL fuer den QR-Code.
// Pre-Fill ist raus (Anti-Pattern, HOTFIX1); URL liegt seit
// HOTFIX2 auf /einloggen.
func (s *Server) buildLoginURL(r *http.Request) string {
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
