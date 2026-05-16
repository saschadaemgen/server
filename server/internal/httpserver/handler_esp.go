package httpserver

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"unifix.local/server/internal/access"
	"unifix.local/server/internal/auth/esptoken"
	"unifix.local/server/internal/mockmanager"
)

// nullable mapped einen leeren string auf SQL-NULL, fuer Spalten
// die optional sind. Kleiner Helper damit die ESP-INSERTs ohne
// COALESCE-Friemelei auskommen.
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

// handleESPDiscover ist der erste Touch eines neuen ESP-Geraets.
// Kein Auth-Header, keine Session. Server speichert MAC + Meta in
// esp_pending_devices; Admin entscheidet im /a/esp-viewers-Tab.
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

	// Wenn das Geraet schon in viewers steht (adoptiert): nichts
	// in esp_pending machen. Status-Poll holt sich den Token aus
	// viewers / dem Handoff-Slot.
	if info, err := s.mockMgr.GetViewerInfo(r.Context(), mac); err == nil && info.Type == mockmanager.TypeESP {
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

// handleESPStatus poll-Endpoint fuer das ESP. Liefert pending /
// adopted / rejected. Beim ersten "adopted"-Hit wird der Klartext-
// Token aus esp_pending_devices.adopted_token_cleartext ausgeliefert
// und der pending-Eintrag dann geloescht. Nachfolgende Polls liefern
// "adopted" OHNE Token (das ESP soll den Token in NVS gespeichert
// haben).
func (s *Server) handleESPStatus(w http.ResponseWriter, r *http.Request) {
	mac := strings.ToLower(r.URL.Query().Get("device_id"))
	if !macFormat.MatchString(mac) {
		http.Error(w, "device_id missing or invalid", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	// Adoption-Check via viewers-Lookup.
	info, vErr := s.mockMgr.GetViewerInfo(r.Context(), mac)
	if vErr == nil && info.Type == mockmanager.TypeESP {
		// Klartext-Token-Handoff einmalig aus pending lesen.
		var tokenCT sql.NullString
		_ = s.sessionsDB().QueryRowContext(r.Context(),
			`SELECT adopted_token_cleartext FROM esp_pending_devices WHERE mac = ?`,
			mac).Scan(&tokenCT)
		// last_poll_at aktualisieren (falls pending row noch da).
		_, _ = s.sessionsDB().ExecContext(r.Context(),
			`UPDATE esp_pending_devices SET last_poll_at = ? WHERE mac = ?`,
			time.Now().UnixMilli(), mac)
		resp := espStatusResponse{Status: "adopted", Config: json.RawMessage(`{}`)}
		if tokenCT.Valid && tokenCT.String != "" {
			resp.Token = tokenCT.String
			// Token wurde abgeholt - pending-Zeile loeschen.
			_, _ = s.sessionsDB().ExecContext(r.Context(),
				`DELETE FROM esp_pending_devices WHERE mac = ?`, mac)
		}
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	// In pending?
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

// espPendingRow ist eine Zeile in der Pending-Liste.
type espPendingRow struct {
	MAC          string `json:"mac"`
	Model        string `json:"model"`
	FwVersion    string `json:"fw_version"`
	DiscoveredAt string `json:"discovered_at"`
	LastPollAt   string `json:"last_poll_at"`
	Rejected     bool   `json:"rejected"`
}

// espAdoptedRow ist eine Zeile in der Adoptiert-Tabelle.
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

// adminESPViewersData ist die Payload fuer templates/admin/esp-viewers.html.
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

// handleAdminESPViewersListJSON liefert die gleichen Daten als
// JSON. Frontend pollt diesen Endpoint alle paar Sekunden um die
// Pending-Liste zu aktualisieren.
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
	// Pending-Liste laden.
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

	// Adoptierte ESP-Viewer aus viewers.
	infos, err := s.mockMgr.ListViewers(r.Context())
	if err != nil {
		return data, err
	}
	userNames := s.lookupUANames(r, espLinkedUserIDs(infos))
	for _, info := range infos {
		if info.Type != mockmanager.TypeESP {
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

func espLinkedUserIDs(infos []mockmanager.ViewerInfo) []string {
	seen := map[string]bool{}
	out := make([]string, 0)
	for _, info := range infos {
		if info.Type != mockmanager.TypeESP {
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

// adoptRequest ist der JSON-Body fuer POST /a/esp-viewers/adopt.
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

// handleAdminESPViewersAdopt verschiebt einen Eintrag aus
// esp_pending_devices in viewers (type='esp') und generiert
// einen Bearer-Token. Der Klartext-Token bleibt in
// esp_pending_devices.adopted_token_cleartext zwischengeparkt
// bis der ESP ihn beim naechsten Status-Poll abholt.
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

	// pending-Eintrag holen (model + fw_version uebernehmen). Falls
	// die Zeile nicht existiert (manueller Adopt), gehen wir mit
	// leeren ESP-Meta-Feldern in viewers - der ESP fuellt sie beim
	// naechsten Discover-Call nach.
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

	spec := mockmanager.ViewerSpec{
		MAC:               mac,
		Name:              name,
		ServicePort:       port,
		Type:              mockmanager.TypeESP,
		LinkedUAUserID:    strings.TrimSpace(body.LinkedUAUserID),
		PairedIntercomMAC: pairedIntercom,
		StreamProfile:     strings.TrimSpace(body.StreamProfile),
		ESPModel:          model.String,
		ESPFwVersion:      fwVersion.String,
		ESPTokenHash:      hash,
	}
	if err := s.mockMgr.AddViewer(r.Context(), spec); err != nil {
		switch {
		case errors.Is(err, mockmanager.ErrMACInUse):
			http.Error(w, "MAC bereits vergeben.", http.StatusConflict)
		case errors.Is(err, mockmanager.ErrNameInUse):
			http.Error(w, "Wohnungs-Name bereits vergeben.", http.StatusConflict)
		default:
			s.log.Error("esp adopt addviewer", "err", err, "mac_prefix", mac[:8])
			http.Error(w, "Adoption fehlgeschlagen.", http.StatusInternalServerError)
		}
		return
	}

	// Token-Handoff-Slot: pending-Zeile (falls vorhanden) updaten,
	// sonst erzeugen. Beim naechsten ESP-Status-Poll wird der
	// Klartext ausgeliefert und die Zeile geloescht.
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

// handleAdminESPViewersReject markiert das pending-Geraet als
// rejected. Bleibt in der Tabelle damit der naechste ESP-Status-
// Poll "rejected" zurueckliefern kann.
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

// handleAdminESPViewersRename benennt den ESP-Viewer um.
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
	if err := s.mockMgr.Rename(r.Context(), mac, name); err != nil {
		if errors.Is(err, mockmanager.ErrViewerNotFound) {
			http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
			return
		}
		if errors.Is(err, mockmanager.ErrNameInUse) {
			http.Error(w, "Name ist bereits vergeben.", http.StatusConflict)
			return
		}
		s.log.Error("esp rename", "err", err, "mac_prefix", mac[:8])
		http.Error(w, "Rename fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/a/esp-viewers", http.StatusSeeOther)
}

// handleAdminESPViewersRegenerateToken erzeugt einen neuen
// Bearer-Token; der alte ist sofort ungueltig. Der ESP holt den
// neuen Klartext beim naechsten Status-Poll.
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
	if err := s.mockMgr.SetESPTokenHash(r.Context(), mac, hash); err != nil {
		if errors.Is(err, mockmanager.ErrViewerNotFound) {
			http.Error(w, "ESP-Viewer nicht gefunden.", http.StatusNotFound)
			return
		}
		s.log.Error("esp regen token store", "err", err, "mac_prefix", mac[:8])
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Klartext in pending-Handoff parken (Token-Pickup beim
	// naechsten Status-Poll).
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

// handleAdminESPViewersDelete loescht einen adoptierten ESP-Viewer.
func (s *Server) handleAdminESPViewersDelete(w http.ResponseWriter, r *http.Request) {
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
		s.log.Error("esp delete", "err", err, "mac_prefix", mac[:8])
		http.Error(w, "Loeschen fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	// pending-Eintrag mit aufraeumen falls vorhanden.
	_, _ = s.sessionsDB().ExecContext(r.Context(),
		`DELETE FROM esp_pending_devices WHERE mac = ?`, mac)
	if r.Method == http.MethodDelete || wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"deleted": mac})
		return
	}
	http.Redirect(w, r, "/a/esp-viewers", http.StatusSeeOther)
}
