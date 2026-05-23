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

	"carvilon.local/server/internal/access"
	"carvilon.local/server/internal/auth/argon2id"
	"carvilon.local/server/internal/auth/loginaudit"
	"carvilon.local/server/internal/normalize"
	"carvilon.local/server/internal/password"
	"carvilon.local/server/internal/platformconfig"
	"carvilon.local/server/internal/viewermanager"
)

// macFormat matches the lowercase colon form, e.g. 0c:ea:14:42:42:42.
var macFormat = regexp.MustCompile(`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`)

// servicePortStart is the first port the auto-allocator hands out.
const servicePortStart = 8100

// adminWebViewersData is the payload for the
// templates/admin/web-viewers.html template (a hand-written
// page, not a library snippet).
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
	infos, err := s.viewerMgr.ListViewers(r.Context())
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
	// Collect the user IDs and resolve them with a single
	// UserStore call so the list can render the link column
	// without N+1 requests.
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
		if info.Type != viewermanager.TypeWeb {
			continue
		}
		nameKey := normalize.ViewerName(info.Name)
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

// credentialsResponse is the JSON envelope returned after create
// / reset.
type credentialsResponse struct {
	MAC      string `json:"mac"`
	Name     string `json:"name"`
	Password string `json:"password"`
	LoginURL string `json:"login_url"`
	QRSVG    string `json:"qr_svg"`
}

// handleAdminWebViewersCreate creates a new web viewer.
//
// The form is intentionally minimal: the Wohnungs-Name is the
// only required input, plus an optional password (empty = the
// server generates one) and an optional linked_ua_user_id. The
// MAC is generated server-side.
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

	spec := viewermanager.ViewerSpec{
		MAC:               mac,
		Name:              name,
		ServicePort:       port,
		Type:              viewermanager.TypeWeb,
		LinkedUAUserID:    linkedUser,
		PairedIntercomMAC: pairedIntercom,
		StreamProfile:     streamProfile,
	}
	if err := s.viewerMgr.AddViewer(r.Context(), spec); err != nil {
		switch {
		case errors.Is(err, viewermanager.ErrMACInUse):
			http.Error(w, "MAC bereits vergeben (RNG-Kollision; bitte erneut anlegen).", http.StatusConflict)
		case errors.Is(err, viewermanager.ErrPortInUse):
			http.Error(w, "Service-Port bereits vergeben.", http.StatusConflict)
		case errors.Is(err, viewermanager.ErrNameInUse):
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

// handleAdminWebViewersResetPW generates a new random password
// and invalidates every existing session for the viewer. Used by
// the "Passwort vergessen" path.
func (s *Server) handleAdminWebViewersResetPW(w http.ResponseWriter, r *http.Request) {
	s.changePassword(w, r, "")
}

// handleAdminWebViewersSetPassword stores a password chosen by
// the admin. Body: { password: "..." }. Same effect as the
// reset-pw path (store the hash, revoke sessions), but the value
// is supplied instead of generated.
//
// No minimum-strength requirements: this is a doorbell deployment,
// not online banking. The admin may type "1234" if they wish.
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

// changePassword is the shared code path for both reset-pw
// (manualPW="") and set-password (manualPW provided).
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, manualPW string) {
	mac := strings.ToLower(r.PathValue("mac"))
	if !macFormat.MatchString(mac) {
		http.Error(w, "ungueltige MAC", http.StatusBadRequest)
		return
	}
	info, err := s.viewerMgr.GetViewerInfo(r.Context(), mac)
	if err != nil {
		if errors.Is(err, viewermanager.ErrViewerNotFound) {
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

// handleAdminWebViewersUnlock lifts the rate-limit lockout on the
// viewer login.
func (s *Server) handleAdminWebViewersUnlock(w http.ResponseWriter, r *http.Request) {
	mac := strings.ToLower(r.PathValue("mac"))
	if !macFormat.MatchString(mac) {
		http.Error(w, "ungueltige MAC", http.StatusBadRequest)
		return
	}
	info, err := s.viewerMgr.GetViewerInfo(r.Context(), mac)
	if err != nil {
		http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
		return
	}
	nameKey := normalize.ViewerName(info.Name)
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

// handleAdminWebViewersRename renames the viewer.
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
	if err := s.viewerMgr.Rename(r.Context(), mac, name); err != nil {
		if errors.Is(err, viewermanager.ErrViewerNotFound) {
			http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
			return
		}
		s.log.Error("rename viewer", "err", err, "mac_prefix", mac[:8])
		http.Error(w, "Rename fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	// Rename changes the UnitName surfaced by the mieter home
	// page and by the /esp/config JSON.
	if s.hub != nil {
		s.hub.BroadcastConfigChanged(r.Context(), mac)
	}
	http.Redirect(w, r, "/a/web-viewers", http.StatusSeeOther)
}

// handleAdminWebViewersEdit is the atomic edit path that touches
// name + password + link in ONE request. Body (form-encoded or
// JSON): name (required), password (empty = keep current),
// linked_ua_user_id (empty = no link).
//
// Replaces the three separate rename / set-password / link
// endpoints that the edit modal used to call. The single-save
// click carries a clear expectation of which fields actually
// changed.
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

	info, err := s.viewerMgr.GetViewerInfo(r.Context(), mac)
	if err != nil {
		if errors.Is(err, viewermanager.ErrViewerNotFound) {
			http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
			return
		}
		s.log.Error("get viewer info", "err", err, "mac_prefix", mac[:8])
		http.Error(w, "Lookup fehlgeschlagen.", http.StatusInternalServerError)
		return
	}

	nameChanged := normalize.ViewerName(info.Name) != normalize.ViewerName(newName)
	pwChanged := newPW != ""
	linkChanged := info.LinkedUAUserID != newLink
	pairedChanged := info.PairedIntercomMAC != newPaired
	streamProfileChanged := info.StreamProfile != newStreamProfile

	if nameChanged {
		if err := s.viewerMgr.Rename(r.Context(), mac, newName); err != nil {
			switch {
			case errors.Is(err, viewermanager.ErrNameInUse):
				http.Error(w,
					"Wohnungs-Name ist bereits vergeben (case-insensitive). Bitte einen anderen Namen waehlen.",
					http.StatusConflict)
			case errors.Is(err, viewermanager.ErrViewerNotFound):
				http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
			default:
				s.log.Error("edit rename", "err", err, "mac_prefix", mac[:8])
				http.Error(w, "Umbenennen fehlgeschlagen.", http.StatusInternalServerError)
			}
			return
		}
	}

	if linkChanged {
		if err := s.viewerMgr.SetLinkedUAUserID(r.Context(), mac, newLink); err != nil {
			s.log.Error("edit set link", "err", err, "mac_prefix", mac[:8])
			http.Error(w, "Verknuepfung fehlgeschlagen.", http.StatusInternalServerError)
			return
		}
	}

	if pairedChanged {
		if err := s.viewerMgr.SetPairedIntercomMAC(r.Context(), mac, newPaired); err != nil {
			s.log.Error("edit set paired intercom", "err", err, "mac_prefix", mac[:8])
			http.Error(w, "Klingel-Verknuepfung fehlgeschlagen.", http.StatusInternalServerError)
			return
		}
	}

	if streamProfileChanged {
		if err := s.viewerMgr.SetStreamProfile(r.Context(), mac, newStreamProfile); err != nil {
			s.log.Error("edit set stream profile", "err", err, "mac_prefix", mac[:8])
			http.Error(w, "Stream-Profil-Speicherung fehlgeschlagen.", http.StatusInternalServerError)
			return
		}
	}

	// Password last, so that a hash failure does not undo the
	// already-stored name and link changes (we have to roll
	// forward anyway because SQLite without SAVEPOINT cannot
	// do a partial rollback here).
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

	// Broadcast config.changed whenever a render-relevant field
	// was touched. A password change invalidates sessions, so
	// the broadcast is intentionally skipped on that branch (the
	// browser is redirected to /login anyway). Stream profile,
	// paired intercom and link all affect the config JSON or the
	// stream URL and therefore trigger the broadcast.
	configChanged := nameChanged || linkChanged || pairedChanged || streamProfileChanged
	if configChanged && s.hub != nil {
		s.hub.BroadcastConfigChanged(r.Context(), mac)
	}

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

// handleAdminWebViewersLoginInfo returns the ready-made tenant
// login link plus a QR-code SVG for one web viewer. Called lazily
// from the edit-modal JS when the modal opens. Both fields come
// out of existing helpers: buildLoginURL() and renderQRSVG().
func (s *Server) handleAdminWebViewersLoginInfo(w http.ResponseWriter, r *http.Request) {
	mac := strings.ToLower(r.PathValue("mac"))
	if !macFormat.MatchString(mac) {
		http.Error(w, "ungueltige MAC", http.StatusBadRequest)
		return
	}
	if _, err := s.viewerMgr.GetViewerInfo(r.Context(), mac); err != nil {
		if errors.Is(err, viewermanager.ErrViewerNotFound) {
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

// handleAdminWebViewersGeneratePW returns a fresh random password
// WITHOUT persisting it. The UI copies the value into the
// password field; the actual write happens on the next
// "Speichern" click in the edit modal.
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

// handleAdminWebViewersSetLink sets or clears
// viewer.linked_ua_user_id. An empty user_id clears the link.
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
	if err := s.viewerMgr.SetLinkedUAUserID(r.Context(), mac, userID); err != nil {
		if errors.Is(err, viewermanager.ErrViewerNotFound) {
			http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
			return
		}
		s.log.Error("set linked user", "err", err, "mac_prefix", mac[:8])
		http.Error(w, "Verknuepfung fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/a/web-viewers", http.StatusSeeOther)
}

// handleAdminWebViewersDelete deletes a viewer; viewer_sessions
// cascade via FK.
func (s *Server) handleAdminWebViewersDelete(w http.ResponseWriter, r *http.Request) {
	mac := strings.ToLower(r.PathValue("mac"))
	if !macFormat.MatchString(mac) {
		http.Error(w, "ungueltige MAC", http.StatusBadRequest)
		return
	}
	if err := s.viewerMgr.RemoveViewer(r.Context(), mac); err != nil {
		if errors.Is(err, viewermanager.ErrViewerNotFound) {
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

// storePasswordForViewer hashes the password with Argon2id +
// pepper, writes the hash, and returns the clear-text password
// plus a QR code. If manualPW is empty, one is generated via
// password.Generate; otherwise the caller-supplied value is
// taken as-is.
//
// No minimum-strength requirements on the admin-typed password:
// doorbell deployment, not online banking. The admin may type
// "1234" if they wish.
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
	if err := s.viewerMgr.SetPasswordHash(ctx, mac, hash); err != nil {
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

// buildLoginURL returns the bare login URL used by the QR code.
// Username pre-fill was removed as an anti-pattern. The URL now
// resolves to /login (the form), with /webviewer/ holding the
// logged-in area after a successful sign-in.
func (s *Server) buildLoginURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/login", scheme, r.Host)
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
