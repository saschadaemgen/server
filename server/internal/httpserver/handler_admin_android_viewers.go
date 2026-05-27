// Admin CRUD for the Android-Viewer tab (Saison 16 Etappe 1).
//
// Android devices are created directly by the admin - there is
// NO discovery / pending flow as with ESP. The handler family
// here is the bare minimum the briefing asks for:
//
//	GET    /a/android-viewers                   list page
//	GET    /a/android-viewers.json              JSON list (optional polling)
//	POST   /a/android-viewers/adopt             create + one-shot token
//	POST   /a/android-viewers/{mac}/regenerate-token  rotate token + reveal
//	POST   /a/android-viewers/{mac}/delete      remove the row
//
// The viewers-row is the same shape ESP uses, plus the new
// device_label column (Migration 018). Type is "android"; the
// viewermanager refuses to spawn a goroutine for it (Etappe 1
// does WebRTC + admin only, doorbell push is FCM in Etappe 2).
//
// One-shot reveal: the cleartext bearer token is returned in the
// adopt / regenerate JSON response exactly once. The DB stores
// only the sha256 hash (via esptoken.Generate, same generator
// the ESP path uses).
package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"carvilon.local/server/internal/auth/esptoken"
	"carvilon.local/server/internal/viewermanager"
)

// adminAndroidViewersData backs templates/admin/android-viewers.html.
type adminAndroidViewersData struct {
	User    adminUser
	Flash   string
	Viewers []androidViewerRow
}

// androidViewerRow is one row in the admin Android-Viewer list.
type androidViewerRow struct {
	MAC               string
	Name              string
	DeviceLabel       string
	LinkedUserID      string
	LinkedUserName    string // resolved via UA-API, empty when unknown
	PairedIntercomMAC string
	StreamProfile     string
	HasToken          bool
}

// androidAdoptRequest is the JSON / form body for the adopt
// endpoint. Mirrors the ESP shape but adds device_label.
type androidAdoptRequest struct {
	Name              string `json:"name"`
	DeviceLabel       string `json:"device_label"`
	LinkedUAUserID    string `json:"linked_ua_user_id"`
	PairedIntercomMAC string `json:"paired_intercom_mac"`
	StreamProfile     string `json:"stream_profile"`
}

// androidAdoptResponse carries the freshly minted cleartext
// token. The client (admin browser) renders it in a reveal
// modal; after this response the server only knows the hash.
type androidAdoptResponse struct {
	MAC          string `json:"mac"`
	Name         string `json:"name"`
	Token        string `json:"token"`
	TokenPreview string `json:"token_preview"`
}

func (s *Server) handleAdminAndroidViewersList(w http.ResponseWriter, r *http.Request) {
	data, err := s.buildAndroidViewersData(r)
	if err != nil {
		s.log.Error("android viewers list", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.renderAdminPage(w, "android-viewers", data)
}

func (s *Server) handleAdminAndroidViewersListJSON(w http.ResponseWriter, r *http.Request) {
	data, err := s.buildAndroidViewersData(r)
	if err != nil {
		s.log.Error("android viewers list json", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"viewers": data.Viewers})
}

func (s *Server) buildAndroidViewersData(r *http.Request) (adminAndroidViewersData, error) {
	username := AdminUserFromContext(r.Context())
	data := adminAndroidViewersData{
		User: adminUser{Name: username, Initials: initialsOf(username)},
	}
	// Direct SQL read instead of going through viewermanager.
	// ListViewers + ViewerInfo - that gives us device_label in
	// one round without expanding ViewerInfo across the manager
	// (which the Etappe-2 FCM-Push commit will do properly).
	rows, err := s.platformCfg.DB().QueryContext(r.Context(),
		`SELECT mac, name, COALESCE(device_label, ''),
		        COALESCE(linked_ua_user_id, ''),
		        COALESCE(paired_intercom_mac, ''),
		        COALESCE(stream_profile, ''),
		        CASE WHEN device_token_hash IS NULL
		                  OR device_token_hash = ''
		             THEN 0 ELSE 1 END
		   FROM viewers
		  WHERE type = 'android'
		  ORDER BY name COLLATE NOCASE, device_label COLLATE NOCASE, mac`)
	if err != nil {
		return data, err
	}
	defer rows.Close()
	linked := map[string]bool{}
	for rows.Next() {
		var row androidViewerRow
		var hasToken int
		if err := rows.Scan(&row.MAC, &row.Name, &row.DeviceLabel,
			&row.LinkedUserID, &row.PairedIntercomMAC,
			&row.StreamProfile, &hasToken); err != nil {
			return data, err
		}
		row.HasToken = hasToken == 1
		if row.LinkedUserID != "" {
			linked[row.LinkedUserID] = true
		}
		data.Viewers = append(data.Viewers, row)
	}
	if err := rows.Err(); err != nil {
		return data, err
	}
	if len(linked) > 0 {
		ids := make([]string, 0, len(linked))
		for id := range linked {
			ids = append(ids, id)
		}
		names := s.lookupUANames(r, ids)
		for i := range data.Viewers {
			if n, ok := names[data.Viewers[i].LinkedUserID]; ok {
				data.Viewers[i].LinkedUserName = n
			}
		}
	}
	return data, nil
}

// handleAdminAndroidViewersAdopt creates a new android viewer row
// with a generated MAC + fresh bearer token. Cleartext token in
// the response is the one-shot reveal; the DB only keeps the
// hash.
func (s *Server) handleAdminAndroidViewersAdopt(w http.ResponseWriter, r *http.Request) {
	body, ok := s.decodeAndroidAdoptBody(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len(name) > 64 {
		http.Error(w, "Name fehlt oder zu lang (max 64 Zeichen).", http.StatusBadRequest)
		return
	}
	paired := strings.ToLower(strings.TrimSpace(body.PairedIntercomMAC))
	if paired != "" && !macFormat.MatchString(paired) {
		http.Error(w, "Klingel-MAC muss lowercase xx:xx:xx:xx:xx:xx sein.", http.StatusBadRequest)
		return
	}

	mac, err := generateUbiquitiMAC()
	if err != nil {
		s.log.Error("android adopt mac", "err", err)
		http.Error(w, "MAC-Generierung fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	port, err := s.nextFreeServicePort(r.Context())
	if err != nil {
		s.log.Error("android adopt port", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	clearText, hash, err := esptoken.Generate()
	if err != nil {
		s.log.Error("android adopt token generate", "err", err)
		http.Error(w, "Token-Erzeugung fehlgeschlagen.", http.StatusInternalServerError)
		return
	}

	spec := viewermanager.ViewerSpec{
		MAC:               mac,
		Name:              name,
		ServicePort:       port,
		Type:              viewermanager.TypeAndroid,
		LinkedUAUserID:    strings.TrimSpace(body.LinkedUAUserID),
		PairedIntercomMAC: paired,
		StreamProfile:     strings.TrimSpace(body.StreamProfile),
		DeviceTokenHash:   hash,
		DeviceLabel:       strings.TrimSpace(body.DeviceLabel),
	}
	if err := s.viewerMgr.AddViewer(r.Context(), spec); err != nil {
		switch {
		case errors.Is(err, viewermanager.ErrMACInUse):
			http.Error(w, "MAC bereits vergeben (RNG-Kollision; bitte erneut anlegen).", http.StatusConflict)
		case errors.Is(err, viewermanager.ErrPortInUse):
			http.Error(w, "Service-Port bereits vergeben.", http.StatusConflict)
		case errors.Is(err, viewermanager.ErrNameInUse):
			http.Error(w, "Name bereits vergeben (case-insensitive).", http.StatusConflict)
		default:
			s.log.Error("android adopt addviewer", "err", err, "mac_prefix", safePrefix(mac))
			http.Error(w, "Anlegen fehlgeschlagen.", http.StatusInternalServerError)
		}
		return
	}
	resp := androidAdoptResponse{
		MAC:          mac,
		Name:         name,
		Token:        clearText,
		TokenPreview: esptoken.Preview(clearText),
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleAdminAndroidViewersRegenerateToken rotates the bearer
// token for one android viewer. Old token is invalidated the
// instant the hash is overwritten in the DB.
func (s *Server) handleAdminAndroidViewersRegenerateToken(w http.ResponseWriter, r *http.Request) {
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
		s.log.Error("android regen lookup", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if info.Type != viewermanager.TypeAndroid {
		http.Error(w, "Kein Android-Viewer.", http.StatusBadRequest)
		return
	}
	clearText, hash, err := esptoken.Generate()
	if err != nil {
		s.log.Error("android regen generate", "err", err)
		http.Error(w, "Token-Erzeugung fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	if err := s.viewerMgr.SetDeviceTokenHash(r.Context(), mac, hash); err != nil {
		s.log.Error("android regen store", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"mac":           mac,
		"token":         clearText,
		"token_preview": esptoken.Preview(clearText),
	})
}

// handleAdminAndroidViewersDelete removes an android viewer row.
// FK cascades clear viewer_sessions / door_events /
// viewer_hidden_events; the viewer never had a goroutine so
// there is nothing to cancel in the manager beyond the row.
func (s *Server) handleAdminAndroidViewersDelete(w http.ResponseWriter, r *http.Request) {
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
		s.log.Error("android delete", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "Loeschen fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	if r.Method == http.MethodDelete || wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"deleted": mac})
		return
	}
	http.Redirect(w, r, "/a/android-viewers", http.StatusSeeOther)
}

// decodeAndroidAdoptBody peels JSON or form-urlencoded into
// androidAdoptRequest. JSON branch is the native-app / fetch
// path the admin UI takes; the form branch is a fallback so a
// curl-style operator can still adopt by hand.
func (s *Server) decodeAndroidAdoptBody(w http.ResponseWriter, r *http.Request) (androidAdoptRequest, bool) {
	var body androidAdoptRequest
	if r.Header.Get("Content-Type") == "application/json" {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return body, false
		}
		return body, true
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return body, false
	}
	body.Name = r.PostForm.Get("name")
	body.DeviceLabel = r.PostForm.Get("device_label")
	body.LinkedUAUserID = r.PostForm.Get("linked_ua_user_id")
	body.PairedIntercomMAC = r.PostForm.Get("paired_intercom_mac")
	body.StreamProfile = r.PostForm.Get("stream_profile")
	return body, true
}

