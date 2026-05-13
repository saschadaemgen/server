// Saison 13-03: Mieter-side call-lifecycle endpoints. Routes
// live under /einloggen/* (the saison-12-FIX4-a-HOTFIX2
// renamed mieter tree) and require an active mieter session.
//
//	POST /einloggen/doors/{id}/unlock   relay UA-API door unlock
//	POST /einloggen/answer              CAS-style answer + cancel-push
//	POST /einloggen/reject              broadcast cancel(reason=rejected)
//	POST /einloggen/end-call            close an answered call
//
// Each handler reads the viewer_mac from the request context
// (set by requireSession) and the active call event_id from the
// JSON body. The event_id is the cancel_token the mieter
// browser received in the doorbell_start SSE frame.
package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"unifix.local/server/internal/doorbellcalls"
	"unifix.local/server/internal/doorhistory"
	"unifix.local/server/internal/eventbus"
	"unifix.local/server/internal/uaapi"
)

// handleMieterUnlock relays a door unlock to the UA-API.
// {door_id} comes from the URL path; the actor in the audit
// row is the viewer's MAC from the session context.
func (s *Server) handleMieterUnlock(w http.ResponseWriter, r *http.Request) {
	viewerMAC := ViewerMACFromContext(r.Context())
	if viewerMAC == "" {
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}
	doorID := r.PathValue("door_id")
	if doorID == "" {
		http.Error(w, "door_id required", http.StatusBadRequest)
		return
	}
	if s.ua == nil {
		http.Error(w, "ua-api not configured", http.StatusServiceUnavailable)
		return
	}
	info, err := s.mockMgr.GetViewerInfo(r.Context(), viewerMAC)
	if err != nil {
		s.log.Error("mieter unlock viewer info", "err", err, "mac_prefix", viewerMAC[:8])
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.ua.UnlockDoor(r.Context(), doorID, uaapi.UnlockDoorRequest{
		ActorID:   info.MAC,
		ActorName: info.Name,
	}); err != nil {
		s.log.Warn("mieter unlock failed", "err", err, "door_id", doorID, "mac_prefix", viewerMAC[:8])
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}
	if s.history != nil {
		_, _ = s.history.Insert(r.Context(), doorhistory.Event{
			MockMAC:    viewerMAC,
			EventType:  "door_unlocked",
			OccurredAt: time.Now(),
		}, nil)
	}
	s.log.Info("mieter unlock", "mac_prefix", viewerMAC[:8], "door_id", doorID)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// callLifecycleRequest is the shared body shape for /answer,
// /reject, /end-call.
type callLifecycleRequest struct {
	EventID string `json:"event_id"`
}

// handleMieterAnswer is the CAS-style answer-arbiter. The
// winning viewer learns it via firstAnswerer=true and is the
// only one that pushes a cancel(reason=answered_elsewhere) to
// every other subscriber on the same MAC. Race-losers learn
// they lost and stay silent on the bus (they will receive the
// winner's cancel themselves via the bus).
func (s *Server) handleMieterAnswer(w http.ResponseWriter, r *http.Request) {
	viewerMAC := ViewerMACFromContext(r.Context())
	if viewerMAC == "" {
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}
	if s.calls == nil {
		http.Error(w, "doorbell calls not configured", http.StatusServiceUnavailable)
		return
	}
	body, err := decodeCallBody(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	first, err := s.calls.MarkAnswered(r.Context(), body.EventID, viewerMAC)
	if err != nil {
		if errors.Is(err, doorbellcalls.ErrCallNotFound) {
			http.Error(w, "call not active", http.StatusConflict)
			return
		}
		s.log.Error("mieter answer mark", "err", err, "event_id", body.EventID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !first {
		http.Error(w, "already answered or expired", http.StatusConflict)
		return
	}
	if s.eventBus != nil {
		s.eventBus.Publish(viewerMAC, eventbus.Event{
			Type: "doorbell.cancel",
			JSON: fmt.Sprintf(`{"event_id":%q,"reason":%q}`,
				body.EventID, doorbellcalls.ReasonAnsweredElsewhere),
		})
	}
	s.log.Info("mieter answer", "mac_prefix", viewerMAC[:8], "event_id", body.EventID)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// handleMieterReject ends the call with reason=rejected and
// pushes a cancel to every subscriber on the viewer's MAC
// (including the rejecter so its overlay closes uniformly).
func (s *Server) handleMieterReject(w http.ResponseWriter, r *http.Request) {
	viewerMAC := ViewerMACFromContext(r.Context())
	if viewerMAC == "" {
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}
	if s.calls == nil {
		http.Error(w, "doorbell calls not configured", http.StatusServiceUnavailable)
		return
	}
	body, err := decodeCallBody(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.calls.MarkRejected(r.Context(), body.EventID, viewerMAC); err != nil {
		if errors.Is(err, doorbellcalls.ErrCallNotFound) {
			// already gone; treat as no-op success so the UI
			// doesn't paint an error toast.
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "note": "stale"})
			return
		}
		s.log.Error("mieter reject mark", "err", err, "event_id", body.EventID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if s.eventBus != nil {
		s.eventBus.Publish(viewerMAC, eventbus.Event{
			Type: "doorbell.cancel",
			JSON: fmt.Sprintf(`{"event_id":%q,"reason":%q}`,
				body.EventID, doorbellcalls.ReasonRejected),
		})
	}
	s.log.Info("mieter reject", "mac_prefix", viewerMAC[:8], "event_id", body.EventID)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// handleMieterEndCall closes an already-answered call. Pushes
// a cancel(reason=user_ended) to siblings.
func (s *Server) handleMieterEndCall(w http.ResponseWriter, r *http.Request) {
	viewerMAC := ViewerMACFromContext(r.Context())
	if viewerMAC == "" {
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}
	if s.calls == nil {
		http.Error(w, "doorbell calls not configured", http.StatusServiceUnavailable)
		return
	}
	body, err := decodeCallBody(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.calls.MarkEnded(r.Context(), body.EventID, viewerMAC, doorbellcalls.ReasonUserEnded); err != nil {
		if errors.Is(err, doorbellcalls.ErrCallNotFound) {
			http.Error(w, "call not active", http.StatusConflict)
			return
		}
		s.log.Error("mieter end-call mark", "err", err, "event_id", body.EventID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if s.eventBus != nil {
		s.eventBus.Publish(viewerMAC, eventbus.Event{
			Type: "doorbell.cancel",
			JSON: fmt.Sprintf(`{"event_id":%q,"reason":%q}`,
				body.EventID, doorbellcalls.ReasonUserEnded),
		})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func decodeCallBody(r *http.Request) (callLifecycleRequest, error) {
	var body callLifecycleRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return body, errors.New("invalid json")
	}
	if body.EventID == "" {
		return body, errors.New("event_id required")
	}
	return body, nil
}

