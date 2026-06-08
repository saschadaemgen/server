// Saison 19-32 Teil C: admin-side door unlock for the per-row
// "Tür öffnen" button in the viewer lists. There was no admin door
// unlock before (handleAdminWebViewersUnlock is a login-lockout
// reset, not a door). requireAdminSession-gated, so the admin is
// trusted and there is NO viewer_doors authorisation here - that gate
// protects the VIEWER-facing path, not the operator.
package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"carvilon.local/server/internal/doorhistory"
	"carvilon.local/server/internal/uaapi"
	"carvilon.local/server/internal/viewermanager"
)

// handleAdminViewerUnlock opens the door of the viewer in {mac}. Door
// selection reuses resolveUnlockDoorID's standby semantics: exactly
// one assigned door opens directly; several need an explicit
// ?door_id= (otherwise 409); none falls back to the paired-intercom
// auto-resolution. An explicitly passed door_id is opened as-is (the
// admin is trusted). The UA audit actor is the admin.
func (s *Server) handleAdminViewerUnlock(w http.ResponseWriter, r *http.Request) {
	mac, ok := parseMACPathValue(w, r)
	if !ok {
		return
	}
	if s.ua == nil {
		http.Error(w, "ua-api not configured", http.StatusServiceUnavailable)
		return
	}
	info, err := s.viewerMgr.GetViewerInfo(r.Context(), mac)
	if err != nil {
		if errors.Is(err, viewermanager.ErrViewerNotFound) {
			http.Error(w, "Viewer nicht gefunden.", http.StatusNotFound)
			return
		}
		s.log.Error("admin unlock get viewer", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	doorID := strings.TrimSpace(r.URL.Query().Get("door_id"))
	if doorID == "" {
		// Standby semantics: one assigned -> it; many -> 409; none -> paired.
		resolved, status, errMsg := s.resolveUnlockDoorID(r.Context(), mac, "standby", info.PairedIntercomMAC)
		if errMsg != "" {
			s.respondAdminUnlockErr(w, status, errMsg)
			return
		}
		doorID = resolved
	}

	admin := AdminUserFromContext(r.Context())
	if err := s.ua.UnlockDoor(r.Context(), doorID, uaapi.UnlockDoorRequest{
		ActorID:   "admin:" + admin,
		ActorName: admin,
	}); err != nil {
		s.log.Warn("admin unlock failed", "err", err, "door_id", doorID, "mac_prefix", safePrefix(mac))
		s.respondAdminUnlockErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if s.history != nil {
		_, _ = s.history.Insert(r.Context(), doorhistory.Event{
			ViewerMAC:  mac,
			EventType:  "door_unlocked",
			OccurredAt: time.Now(),
		}, nil)
	}
	s.log.Info("admin unlock", "mac_prefix", safePrefix(mac), "door_id", doorID, "admin", admin)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// respondAdminUnlockErr mirrors the mieter-unlock JSON error shape so
// the table button can show the server's message (incl. the 409 for
// "multiple doors - pick one").
func (s *Server) respondAdminUnlockErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg})
}
