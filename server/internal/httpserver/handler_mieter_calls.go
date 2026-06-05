// Mieter-side call-lifecycle endpoints. Routes live under
// /webviewer/* (renamed from the legacy /einloggen/* tree) and
// require an active mieter session.
//
//	POST /webviewer/doors/{id}/unlock   relay UA-API door unlock
//	POST /webviewer/answer              CAS-style answer + cancel-push
//	POST /webviewer/reject              broadcast cancel(reason=rejected)
//	POST /webviewer/end-call            close an answered call
//
// Each handler reads the viewer_mac from the request context
// (set by requireViewerAuth) and the active call event_id from the
// JSON body. The event_id is the cancel_token the mieter
// browser received in the doorbell_start SSE frame.
package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"carvilon.local/server/internal/doorbellcalls"
	"carvilon.local/server/internal/doorhistory"
	"carvilon.local/server/internal/eventbus"
	"carvilon.local/server/internal/uaapi"
)

// handleMieterUnlock relays a door unlock to the UA-API. Two
// path parameters are accepted:
//
//   - "standby": the literal string. Reads the viewer's
//     paired_intercom_mac (set by the admin) and uses that as
//     the intercom. The standby button on the home screen
//     POSTs to /webviewer/doors/standby/unlock.
//
//   - <intercom-mac>: a MAC address in either colon-form or
//     bare 12-hex. The bell-overlay POSTs to
//     /webviewer/doors/{device_id}/unlock with the intercom
//     MAC carried in the SSE doorbell_start.device_id frame.
//
// In both branches the door UUID is auto-resolved via
// uaapi.LookupDoorForIntercom: the UA-API's extras.door_thumbnail
// field embeds the calling intercom MAC, so no admin-curated
// platform_config mapping is needed.
func (s *Server) handleMieterUnlock(w http.ResponseWriter, r *http.Request) {
	viewerMAC := ViewerMACFromContext(r.Context())
	if viewerMAC == "" {
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}
	pathParam := r.PathValue("door_id")
	if pathParam == "" {
		http.Error(w, "door_id required", http.StatusBadRequest)
		return
	}
	if s.ua == nil {
		http.Error(w, "ua-api not configured", http.StatusServiceUnavailable)
		return
	}

	info, err := s.viewerMgr.GetViewerInfo(r.Context(), viewerMAC)
	if err != nil {
		s.log.Error("mieter unlock viewer info", "err", err, "mac_prefix", viewerMAC[:8])
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	doorID, status, errMsg := s.resolveUnlockDoorID(r.Context(), viewerMAC, pathParam, info.PairedIntercomMAC)
	if errMsg != "" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": errMsg})
		return
	}

	if err := s.ua.UnlockDoor(r.Context(), doorID, uaapi.UnlockDoorRequest{
		ActorID:   info.MAC,
		ActorName: info.Name,
	}); err != nil {
		s.log.Warn("mieter unlock failed",
			"err", err, "door_id", doorID, "mac_prefix", viewerMAC[:8])
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
			ViewerMAC:  viewerMAC,
			EventType:  "door_unlocked",
			OccurredAt: time.Now(),
		}, nil)
	}
	s.log.Info("mieter unlock",
		"mac_prefix", viewerMAC[:8],
		"door_id", doorID,
		"via", pathParam,
	)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// resolveUnlockDoorID maps the {door_id} path param to the concrete
// UA door UUID to open and applies the Saison 19-30 authorisation.
// Returns (doorID, 0, "") on success, or ("", httpStatus, message)
// to reject. Three branches:
//
//   - explicit door UUID: authorise against viewer_doors - a viewer
//     may only open doors an admin assigned (else 403). This is the
//     path the /webviewer/doors buttons use.
//   - "standby": the viewer's 1:n assignment. Exactly one assigned
//     door opens directly; several -> 409 (the client must send the
//     concrete UUID); none -> legacy paired-intercom auto-resolution
//     so single-bell setups keep working.
//   - intercom MAC (the live SSE device_id): in-call auto-resolution
//     via extras.door_thumbnail, UNCHANGED.
func (s *Server) resolveUnlockDoorID(ctx context.Context, viewerMAC, pathParam, pairedIntercom string) (string, int, string) {
	if pathParam == "standby" {
		assigned, err := s.viewerMgr.ListViewerDoors(ctx, viewerMAC)
		if err != nil {
			s.log.Error("unlock list viewer doors", "err", err, "mac_prefix", safePrefix(viewerMAC))
			return "", http.StatusInternalServerError, "internal error"
		}
		switch {
		case len(assigned) == 1:
			return assigned[0].DoorID, 0, ""
		case len(assigned) > 1:
			return "", http.StatusConflict, "door_id required (multiple doors assigned)"
		default:
			// No assignment yet: fall back to the legacy paired-
			// intercom auto-resolution (also covers the in-call
			// standby path on single-bell setups).
			return s.resolveDoorViaIntercom(ctx, pairedIntercom)
		}
	}
	if macAnyForm.MatchString(pathParam) {
		return s.resolveDoorViaIntercom(ctx, normalizeMACToColonForm(pathParam))
	}
	// Otherwise: treat the param as a direct UA door UUID. AUTHORISE
	// it - critical security gate, a viewer may only open doors that
	// an admin assigned in viewer_doors.
	ok, err := s.viewerMgr.ViewerHasDoor(ctx, viewerMAC, pathParam)
	if err != nil {
		s.log.Error("unlock authz check", "err", err, "mac_prefix", safePrefix(viewerMAC))
		return "", http.StatusInternalServerError, "internal error"
	}
	if !ok {
		s.log.Warn("unlock denied: door not assigned to viewer",
			"mac_prefix", safePrefix(viewerMAC), "door_id", pathParam)
		return "", http.StatusForbidden, "door not assigned to this viewer"
	}
	return pathParam, 0, ""
}

// resolveDoorViaIntercom is the legacy intercom-MAC -> door-UUID
// auto-resolution (extras.door_thumbnail). Shared by the standby
// zero-assignment fallback and the in-call MAC branch.
func (s *Server) resolveDoorViaIntercom(ctx context.Context, intercomMAC string) (string, int, string) {
	if intercomMAC == "" {
		return "", http.StatusNotFound,
			"viewer has no door assigned and no paired intercom; admin must assign a door (Tuer-Zuordnung)"
	}
	doorID, err := s.ua.LookupDoorForIntercom(ctx, intercomMAC)
	if err != nil {
		s.log.Error("ua-api door lookup failed", "err", err, "intercom", intercomMAC)
		return "", http.StatusBadGateway, "ua-api door lookup failed: " + err.Error()
	}
	if doorID == "" {
		s.log.Warn("intercom not bound to any door (UA-Console misconfiguration)", "intercom", intercomMAC)
		return "", http.StatusNotFound, "intercom is not bound to any door (check UA-Console)"
	}
	return doorID, 0, ""
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
// The intercom MAC comes from the doorbell_calls row's device_id
// (populated by doorbellhub.startCall when the /remote_view RPC
// arrived).
func (s *Server) notifyUDMReject(ctx context.Context, eventID, viewerMAC string) {
	if s.calls == nil || s.viewerMgr == nil {
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
	if err := s.viewerMgr.RejectDoorbellOnViewer(viewerMAC, call.DeviceID); err != nil {
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
