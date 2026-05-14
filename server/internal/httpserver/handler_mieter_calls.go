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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"unifix.local/server/internal/doorbellcalls"
	"unifix.local/server/internal/doorhistory"
	"unifix.local/server/internal/eventbus"
	"unifix.local/server/internal/uaapi"
)

// handleMieterUnlock relays a door unlock to the UA-API.
// {door_id} comes from the URL path; the actor in the audit
// row is the viewer's MAC from the session context.
//
// Saison 13-03-HOTFIX1: the path parameter the bell-overlay JS
// sends is the INTERCOM MAC (from doorbell_start.device_id),
// not the UA-Access door UUID. We sniff the format: if the
// param matches the MAC regex, look it up via
// platformconfig.LookupDoorForIntercom and use the resolved
// UUID; otherwise treat the param as a real door UUID
// (preserves the test path that calls /einloggen/doors/door-x/unlock
// with a synthetic id and the future admin path that may use
// the UUID directly).
func (s *Server) handleMieterUnlock(w http.ResponseWriter, r *http.Request) {
	viewerMAC := ViewerMACFromContext(r.Context())
	if viewerMAC == "" {
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}
	pathDoorID := r.PathValue("door_id")
	if pathDoorID == "" {
		http.Error(w, "door_id required", http.StatusBadRequest)
		return
	}
	if s.ua == nil {
		http.Error(w, "ua-api not configured", http.StatusServiceUnavailable)
		return
	}

	doorID := pathDoorID
	if macFormat.MatchString(strings.ToLower(pathDoorID)) {
		resolved, err := s.platformCfg.LookupDoorForIntercom(r.Context(), pathDoorID)
		if err != nil {
			s.log.Error("intercom_to_door lookup", "err", err, "intercom", pathDoorID)
			http.Error(w, "intercom-to-door mapping read failed", http.StatusInternalServerError)
			return
		}
		if resolved == "" {
			s.log.Warn("intercom not in intercom_to_door mapping", "intercom", pathDoorID)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":    false,
				"error": "intercom MAC not mapped to a door; admin must set platform_config.intercom_to_door",
			})
			return
		}
		doorID = resolved
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
	s.notifyUDMReject(r.Context(), body.EventID, viewerMAC)
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
	s.notifyUDMReject(r.Context(), body.EventID, viewerMAC)
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

// notifyUDMReject asks the mock viewer for viewerMAC to publish a
// /call_admin_result RPC to UDM so the intercom stops ringing
// immediately rather than waiting for the 30-second hardware
// timeout. Best-effort: any failure is logged but never bubbled
// up to the browser - the local lifecycle is already correct and
// the intercom will time out on its own as fallback.
//
// Saison 13-04.5-B. The intercom MAC comes from the doorbell_calls
// row's device_id (populated by doorbellhub.startCall when the
// /remote_view RPC arrived).
func (s *Server) notifyUDMReject(ctx context.Context, eventID, viewerMAC string) {
	if s.calls == nil || s.mockMgr == nil {
		return
	}
	call, err := s.calls.Get(ctx, eventID)
	if err != nil {
		s.log.Warn("call_admin_result lookup failed",
			"event_id", eventID,
			"err", err,
		)
		return
	}
	if call.DeviceID == "" {
		s.log.Info("call_admin_result skipped: call has no device_id",
			"event_id", eventID,
		)
		return
	}
	if err := s.mockMgr.RejectDoorbellOnMock(viewerMAC, call.DeviceID); err != nil {
		s.log.Warn("call_admin_result publish failed",
			"viewer_mac_prefix", safePrefix(viewerMAC),
			"intercom", call.DeviceID,
			"event_id", eventID,
			"err", err,
		)
		return
	}
	s.log.Info("call_admin_result sent to UDM",
		"viewer_mac_prefix", safePrefix(viewerMAC),
		"intercom", call.DeviceID,
		"event_id", eventID,
	)
}


