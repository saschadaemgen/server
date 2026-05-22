package httpserver

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"carvilon.local/server/internal/access"
	"carvilon.local/server/internal/auth/esptoken"
	"carvilon.local/server/internal/viewermanager"
)

// nullable maps an empty string to SQL NULL for optional columns.
// Small helper so the ESP INSERTs do not need COALESCE plumbing.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---------- Public Discovery API (/esp/) ----------

type espDiscoverRequest struct {
	MAC          string   `json:"mac"`
	Model        string   `json:"model"`
	FwVersion    string   `json:"fw_version"`
	Capabilities []string `json:"capabilities"`
}

type espStatusResponse struct {
	Status string          `json:"status"`           // "pending" | "adopted" | "rejected"
	Token  string          `json:"token,omitempty"`  // nur bei "adopted" und nur EINMAL
	Config json.RawMessage `json:"config,omitempty"` // FIX4-d fuellt das mit echten Werten
}

// handleESPDiscover is the first touch of a new ESP device. No
// auth header, no session. The server stores MAC + meta in
// esp_pending_devices; the admin decides in the /a/esp-viewers
// tab.
func (s *Server) handleESPDiscover(w http.ResponseWriter, r *http.Request) {
	var body espDiscoverRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	mac := strings.ToLower(strings.TrimSpace(body.MAC))
	if !macFormat.MatchString(mac) {
		http.Error(w, "mac must be lowercase xx:xx:xx:xx:xx:xx", http.StatusBadRequest)
		return
	}
	capsJSON, _ := json.Marshal(body.Capabilities)

	// If the device is already in viewers (adopted): do not touch
	// esp_pending. The status poll fetches its token from
	// viewers / the handoff slot.
	if info, err := s.viewerMgr.GetViewerInfo(r.Context(), mac); err == nil && info.Type == viewermanager.TypeESP {
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_id": mac,
			"status":    "already_adopted",
		})
		return
	}

	now := time.Now().UnixMilli()
	_, err := s.sessionsDB().ExecContext(r.Context(),
		`INSERT INTO esp_pending_devices
		   (mac, model, fw_version, capabilities, discovered_at, last_poll_at, rejected_at, adopted_token_cleartext)
		 VALUES (?, ?, ?, ?, ?, ?, NULL, NULL)
		 ON CONFLICT(mac) DO UPDATE SET
		   model           = excluded.model,
		   fw_version      = excluded.fw_version,
		   capabilities    = excluded.capabilities,
		   last_poll_at    = excluded.last_poll_at,
		   rejected_at     = NULL`,
		mac, nullable(body.Model), nullable(body.FwVersion), string(capsJSON), now, now,
	)
	if err != nil {
		s.log.Error("esp discover insert", "err", err, "mac_prefix", mac[:8])
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"device_id": mac,
		"status":    "pending",
	})
}

// handleESPStatus is the poll endpoint for the ESP. Returns
// pending / adopted / rejected. On the first "adopted" hit the
// cleartext token from esp_pending_devices.adopted_token_cleartext
// is delivered and the pending row is then deleted. Subsequent
// polls return "adopted" WITHOUT a token (the ESP is expected to
// have stored it in NVS).
func (s *Server) handleESPStatus(w http.ResponseWriter, r *http.Request) {
	mac := strings.ToLower(r.URL.Query().Get("device_id"))
	if !macFormat.MatchString(mac) {
		http.Error(w, "device_id missing or invalid", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	// Adoption check via viewers lookup.
	info, vErr := s.viewerMgr.GetViewerInfo(r.Context(), mac)
	if vErr == nil && info.Type == viewermanager.TypeESP {
		// Read the cleartext token handoff once from pending.
		var tokenCT sql.NullString
		_ = s.sessionsDB().QueryRowContext(r.Context(),
			`SELECT adopted_token_cleartext FROM esp_pending_devices WHERE mac = ?`,
			mac).Scan(&tokenCT)
		// Update last_poll_at (if the pending row still exists).
		_, _ = s.sessionsDB().ExecContext(r.Context(),
			`UPDATE esp_pending_devices SET last_poll_at = ? WHERE mac = ?`,
			time.Now().UnixMilli(), mac)
		resp := espStatusResponse{Status: "adopted", Config: json.RawMessage(`{}`)}
		if tokenCT.Valid && tokenCT.String != "" {
			resp.Token = tokenCT.String
			// Token picked up - delete the pending row.
			_, _ = s.sessionsDB().ExecContext(r.Context(),
				`DELETE FROM esp_pending_devices WHERE mac = ?`, mac)
		}
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	// In pending?
	// (still queued, not yet adopted by the admin).
	var (
		rejectedAt sql.NullInt64
	)
	err := s.sessionsDB().QueryRowContext(r.Context(),
		`SELECT rejected_at FROM esp_pending_devices WHERE mac = ?`,
		mac).Scan(&rejectedAt)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, `{"error":"device_id not found"}`, http.StatusNotFound)
		return
	}
	if err != nil {
		s.log.Error("esp status select", "err", err, "mac_prefix", mac[:8])
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	_, _ = s.sessionsDB().ExecContext(r.Context(),
		`UPDATE esp_pending_devices SET last_poll_at = ? WHERE mac = ?`,
		time.Now().UnixMilli(), mac)
	if rejectedAt.Valid {
		_ = json.NewEncoder(w).Encode(espStatusResponse{Status: "rejected"})
		return
	}
	_ = json.NewEncoder(w).Encode(espStatusResponse{Status: "pending"})
}

// ---------- Admin (/a/esp-viewers) ----------

// espPendingRow is one row in the pending list.
type espPendingRow struct {
	MAC          string `json:"mac"`
	Model        string `json:"model"`
	FwVersion    string `json:"fw_version"`
	DiscoveredAt string `json:"discovered_at"`
	LastPollAt   string `json:"last_poll_at"`
	Rejected     bool   `json:"rejected"`
}

// espAdoptedRow is one row in the adopted table.
type espAdoptedRow struct {
	MAC            string `json:"mac"`
	Name           string `json:"name"`
	Model          string `json:"model"`
	FwVersion      string `json:"fw_version"`
	HasToken       bool   `json:"has_token"`
	LinkedUserID   string `json:"linked_ua_user_id"`
	LinkedUserName string `json:"linked_user_name"`
	StreamProfile  string `json:"stream_profile"`
}

// adminESPViewersData is the payload for templates/admin/esp-viewers.html.
type adminESPViewersData struct {
	User      adminUser
	Pending   []espPendingRow
	Adopted   []espAdoptedRow
	Flash     string
	FlashType string
}

func (s *Server) handleAdminESPViewersList(w http.ResponseWriter, r *http.Request) {
	data, err := s.buildESPViewersData(r)
	if err != nil {
		s.log.Error("esp viewers list", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.renderAdminPage(w, "esp-viewers", data)
}

// handleAdminESPViewersListJSON returns the same data as JSON.
// The frontend polls this endpoint every few seconds to keep
// the pending list fresh.
func (s *Server) handleAdminESPViewersListJSON(w http.ResponseWriter, r *http.Request) {
	data, err := s.buildESPViewersData(r)
	if err != nil {
		s.log.Error("esp viewers list json", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"pending": data.Pending,
		"adopted": data.Adopted,
	})
}

func (s *Server) buildESPViewersData(r *http.Request) (adminESPViewersData, error) {
	adminName := AdminUserFromContext(r.Context())
	data := adminESPViewersData{
		User: adminUser{Name: adminName, Initials: initialsOf(adminName)},
	}
	// Load the pending list.
	rows, err := s.sessionsDB().QueryContext(r.Context(),
		`SELECT mac, COALESCE(model, ''), COALESCE(fw_version, ''),
		        discovered_at, last_poll_at, rejected_at
		   FROM esp_pending_devices
		  ORDER BY discovered_at DESC`)
	if err != nil {
		return data, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			row        espPendingRow
			discovered int64
			lastPoll   int64
			rejected   sql.NullInt64
		)
		if err := rows.Scan(&row.MAC, &row.Model, &row.FwVersion,
			&discovered, &lastPoll, &rejected); err != nil {
			return data, err
		}
		row.DiscoveredAt = time.UnixMilli(discovered).Local().Format("02.01. 15:04")
		row.LastPollAt = time.UnixMilli(lastPoll).Local().Format("02.01. 15:04")
		row.Rejected = rejected.Valid
		data.Pending = append(data.Pending, row)
	}
	if err := rows.Err(); err != nil {
		return data, err
	}

	// Adopted ESP viewers from the viewers table.
	infos, err := s.viewerMgr.ListViewers(r.Context())
	if err != nil {
		return data, err
	}
	userNames := s.lookupUANames(r, espLinkedUserIDs(infos))
	for _, info := range infos {
		if info.Type != viewermanager.TypeESP {
			continue
		}
		row := espAdoptedRow{
			MAC:           info.MAC,
			Name:          info.Name,
			Model:         info.ESPModel,
			FwVersion:     info.ESPFwVersion,
			HasToken:      info.HasESPToken,
			LinkedUserID:  info.LinkedUAUserID,
			StreamProfile: info.StreamProfile,
		}
		if name, ok := userNames[info.LinkedUAUserID]; ok {
			row.LinkedUserName = name
		}
		data.Adopted = append(data.Adopted, row)
	}
	return data, nil
}

func espLinkedUserIDs(infos []viewermanager.ViewerInfo) []string {
	seen := map[string]bool{}
	out := make([]string, 0)
	for _, info := range infos {
		if info.Type != viewermanager.TypeESP {
			continue
		}
		if info.LinkedUAUserID == "" || seen[info.LinkedUAUserID] {
			continue
		}
		seen[info.LinkedUAUserID] = true
		out = append(out, info.LinkedUAUserID)
	}
	return out
}

func (s *Server) lookupUANames(r *http.Request, ids []string) map[string]string {
	out := map[string]string{}
	if s.userStore == nil || !s.userStore.IsConfigured() || len(ids) == 0 {
		return out
	}
	needed := map[string]bool{}
	for _, id := range ids {
		needed[id] = true
	}
	res, err := s.userStore.List(r.Context(), access.ListParams{Page: 1, Size: usersMaxPageSize})
	if err != nil {
		return out
	}
	for _, u := range res.Users {
		if needed[u.ID] {
			out[u.ID] = u.DisplayName()
		}
	}
	return out
}

// espAdoptRequest is the JSON body for POST /a/esp-viewers/adopt.
type espAdoptRequest struct {
	MAC               string `json:"mac"`
	Name              string `json:"name"`
	LinkedUAUserID    string `json:"linked_ua_user_id"`
	PairedIntercomMAC string `json:"paired_intercom_mac"`
	StreamProfile     string `json:"stream_profile"`
}

type espAdoptResponse struct {
	MAC          string `json:"mac"`
	Name         string `json:"name"`
	Model        string `json:"model"`
	TokenPreview string `json:"token_preview"`
}

// handleAdminESPViewersAdopt moves a row from esp_pending_devices
// into viewers (type='esp') and generates a bearer token. The
// cleartext token is parked in
// esp_pending_devices.adopted_token_cleartext until the ESP
// picks it up on its next status poll.
func (s *Server) handleAdminESPViewersAdopt(w http.ResponseWriter, r *http.Request) {
	var body espAdoptRequest
	if r.Header.Get("Content-Type") == "application/json" {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
	} else {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		body.MAC = r.PostForm.Get("mac")
		body.Name = r.PostForm.Get("name")
		body.LinkedUAUserID = r.PostForm.Get("linked_ua_user_id")
		body.PairedIntercomMAC = r.PostForm.Get("paired_intercom_mac")
		body.StreamProfile = r.PostForm.Get("stream_profile")
	}

	mac := strings.ToLower(strings.TrimSpace(body.MAC))
	name := strings.TrimSpace(body.Name)
	pairedIntercom := strings.ToLower(strings.TrimSpace(body.PairedIntercomMAC))
	if !macFormat.MatchString(mac) {
		http.Error(w, "MAC muss lowercase xx:xx:xx:xx:xx:xx sein.", http.StatusBadRequest)
		return
	}
	if name == "" || len(name) > 64 {
		http.Error(w, "Name fehlt oder zu lang.", http.StatusBadRequest)
		return
	}
	if pairedIntercom != "" && !macFormat.MatchString(pairedIntercom) {
		http.Error(w, "Klingel-MAC muss lowercase xx:xx:xx:xx:xx:xx sein.", http.StatusBadRequest)
		return
	}

	// Fetch the pending entry (carry model + fw_version through).
	// If the row does not exist (manual adopt), we proceed with
	// empty ESP-meta fields in viewers - the ESP fills them in on
	// its next discover call.
	var model, fwVersion sql.NullString
	err := s.sessionsDB().QueryRowContext(r.Context(),
		`SELECT COALESCE(model,''), COALESCE(fw_version,'') FROM esp_pending_devices WHERE mac = ?`,
		mac).Scan(&model, &fwVersion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.log.Error("esp adopt pending lookup", "err", err, "mac_prefix", mac[:8])
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	clearText, hash, err := esptoken.Generate()
	if err != nil {
		s.log.Error("esp token generate", "err", err)
		http.Error(w, "Token-Erzeugung fehlgeschlagen.", http.StatusInternalServerError)
		return
	}

	port, err := s.nextFreeServicePort(r.Context())
	if err != nil {
		s.log.Error("esp adopt port", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	spec := viewermanager.ViewerSpec{
		MAC:               mac,
		Name:              name,
		ServicePort:       port,
		Type:              viewermanager.TypeESP,
		LinkedUAUserID:    strings.TrimSpace(body.LinkedUAUserID),
		PairedIntercomMAC: pairedIntercom,
		StreamProfile:     strings.TrimSpace(body.StreamProfile),
		ESPModel:          model.String,
		ESPFwVersion:      fwVersion.String,
		ESPTokenHash:      hash,
	}
	if err := s.viewerMgr.AddViewer(r.Context(), spec); err != nil {
		switch {
		case errors.Is(err, viewermanager.ErrMACInUse):
			http.Error(w, "MAC bereits vergeben.", http.StatusConflict)
		case errors.Is(err, viewermanager.ErrNameInUse):
			http.Error(w, "Wohnungs-Name bereits vergeben.", http.StatusConflict)
		default:
			s.log.Error("esp adopt addviewer", "err", err, "mac_prefix", mac[:8])
			http.Error(w, "Adoption fehlgeschlagen.", http.StatusInternalServerError)
		}
		return
	}

	// Token-handoff slot: update the pending row (if present),
	// otherwise create it. On the next ESP status poll the
	// cleartext is delivered and the row is deleted.
	now := time.Now().UnixMilli()
	_, err = s.sessionsDB().ExecContext(r.Context(),
		`INSERT INTO esp_pending_devices
		   (mac, model, fw_version, capabilities, discovered_at, last_poll_at, adopted_token_cleartext)
		 VALUES (?, ?, ?, '[]', ?, ?, ?)
		 ON CONFLICT(mac) DO UPDATE SET
		   adopted_token_cleartext = excluded.adopted_token_cleartext,
		   rejected_at             = NULL`,
		mac, nullable(model.String), nullable(fwVersion.String), now, now, clearText,
	)
	if err != nil {
		s.log.Error("esp adopt handoff", "err", err)
	}

	resp := espAdoptResponse{
		MAC:          mac,
		Name:         name,
		Model:        model.String,
		TokenPreview: esptoken.Preview(clearText),
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleAdminESPViewersReject marks the pending device as
// rejected. It stays in the table so the next ESP status poll
// can return "rejected".
func (s *Server) handleAdminESPViewersReject(w http.ResponseWriter, r *http.Request) {
	mac := strings.ToLower(r.PathValue("mac"))
	if !macFormat.MatchString(mac) {
		http.Error(w, "ungueltige MAC", http.StatusBadRequest)
		return
	}
	res, err := s.sessionsDB().ExecContext(r.Context(),
		`UPDATE esp_pending_devices SET rejected_at = ? WHERE mac = ?`,
		time.Now().UnixMilli(), mac)
	if err != nil {
		s.log.Error("esp reject", "err", err, "mac_prefix", mac[:8])
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		http.Error(w, "Geraet nicht in der Pending-Liste.", http.StatusNotFound)
		return
	}
	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"rejected": mac})
		return
	}
	http.Redirect(w, r, "/a/esp-viewers", http.StatusSeeOther)
}

// handleAdminESPViewersRename renames the ESP viewer.
func (s *Server) handleAdminESPViewersRename(w http.ResponseWriter, r *http.Request) {
	mac := strings.ToLower(r.PathValue("mac"))
	if !macFormat.MatchString(mac) {
		http.Error(w, "ungueltige MAC", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
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
		if errors.Is(err, viewermanager.ErrNameInUse) {
			http.Error(w, "Name ist bereits vergeben.", http.StatusConflict)
			return
		}
		s.log.Error("esp rename", "err", err, "mac_prefix", mac[:8])
		http.Error(w, "Rename fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	// The ESP device pulls the new UnitName via /esp/config.
	if s.hub != nil {
		s.hub.BroadcastConfigChanged(r.Context(), mac)
	}
	http.Redirect(w, r, "/a/esp-viewers", http.StatusSeeOther)
}

// handleAdminESPViewersRegenerateToken generates a new bearer
// token; the old one is invalid immediately. The ESP picks up
// the new cleartext on its next status poll.
func (s *Server) handleAdminESPViewersRegenerateToken(w http.ResponseWriter, r *http.Request) {
	mac := strings.ToLower(r.PathValue("mac"))
	if !macFormat.MatchString(mac) {
		http.Error(w, "ungueltige MAC", http.StatusBadRequest)
		return
	}
	clearText, hash, err := esptoken.Generate()
	if err != nil {
		s.log.Error("esp regen token generate", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.viewerMgr.SetESPTokenHash(r.Context(), mac, hash); err != nil {
		if errors.Is(err, viewermanager.ErrViewerNotFound) {
			http.Error(w, "ESP-Viewer nicht gefunden.", http.StatusNotFound)
			return
		}
		s.log.Error("esp regen token store", "err", err, "mac_prefix", mac[:8])
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Park the cleartext in the pending handoff slot (the ESP
	// picks it up on its next status poll).
	now := time.Now().UnixMilli()
	_, err = s.sessionsDB().ExecContext(r.Context(),
		`INSERT INTO esp_pending_devices
		   (mac, discovered_at, last_poll_at, adopted_token_cleartext)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(mac) DO UPDATE SET
		   adopted_token_cleartext = excluded.adopted_token_cleartext,
		   last_poll_at = excluded.last_poll_at,
		   rejected_at = NULL`,
		mac, now, now, clearText,
	)
	if err != nil {
		s.log.Warn("esp regen handoff", "err", err)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"mac":           mac,
		"token_preview": esptoken.Preview(clearText),
	})
}

// handleAdminESPViewersDelete deletes an adopted ESP viewer.
func (s *Server) handleAdminESPViewersDelete(w http.ResponseWriter, r *http.Request) {
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
		s.log.Error("esp delete", "err", err, "mac_prefix", mac[:8])
		http.Error(w, "Loeschen fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	// Sweep up the pending row too if it still exists.
	_, _ = s.sessionsDB().ExecContext(r.Context(),
		`DELETE FROM esp_pending_devices WHERE mac = ?`, mac)
	if r.Method == http.MethodDelete || wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"deleted": mac})
		return
	}
	http.Redirect(w, r, "/a/esp-viewers", http.StatusSeeOther)
}
