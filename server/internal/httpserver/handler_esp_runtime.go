// Package httpserver - protected /esp/ endpoint family.
//
// Saison 13-02-FIX4-d: every handler in this file requires the
// caller to be an adopted ESP-Viewer. The bearer-auth middleware
// (middleware_esp.go) puts the matched viewer MAC on the request
// context; handlers read it via ESPMACFromContext.
//
// Endpoints in this file (registered in server.go):
//
//	GET  /esp/config        snapshot of mieter / stream / doors / ui
//	GET  /esp/events        SSE long-stream
//	GET  /esp/heartbeat     fallback when SSE is blocked
//	POST /esp/answer        accept / reject the active doorbell
//	POST /esp/reject        dedicated reject (saison-13-08; calls
//	                        doorbellcalls service + UDM ring-stop)
//	POST /esp/unlock        relay an UA-API door unlock
//	POST /esp/state         ESP-side status report
package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"unifix.local/server/internal/doorbellcalls"
	"unifix.local/server/internal/eventbus"
	"unifix.local/server/internal/mockmanager"
	"unifix.local/server/internal/uaapi"
)

// uaapiUnlockReq builds the actor block UA-API sees for an
// ESP-driven unlock. The ESP's MAC is the stable identifier;
// the viewer name is the human-readable display label.
func uaapiUnlockReq(info *mockmanager.ViewerInfo) uaapi.UnlockDoorRequest {
	if info == nil {
		return uaapi.UnlockDoorRequest{}
	}
	return uaapi.UnlockDoorRequest{
		ActorID:   info.MAC,
		ActorName: info.Name,
	}
}

// espHeartbeatInterval is how often the SSE stream emits a
// "heartbeat" event when there is no real traffic. Most home
// routers and HTTPS-terminating proxies drop idle TCP after
// 60 seconds; 30 keeps us comfortably under that.
const espHeartbeatInterval = 30 * time.Second

// espConfigResponse is the JSON shape returned by GET /esp/config.
// Fields the ESP firmware reads to draw its UI and connect to the
// stream. Several values are pulled from the per-viewer row in
// the viewers table (mieter_name); others - stream URL, door
// list, location name - are still defaulted in FIX4-d because
// the UA-API client and the go2rtc-config integration are not
// wired yet. Defaults match the constants the ESP-side mock
// firmware is being written against in the parallel ESP-Saison-2.
type espConfigResponse struct {
	MieterName   string         `json:"mieter_name"`
	LocationName string         `json:"location_name"`
	Stream       espStream      `json:"stream"`
	Doors        []espDoor      `json:"doors"`
	Cameras      []espCamera    `json:"cameras"`
	UI           espUISettings  `json:"ui"`
}

type espStream struct {
	URL         string `json:"url"`
	Type        string `json:"type"`
	AuthHeader  string `json:"auth_header"`
	FallbackURL string `json:"fallback_url"`
}

type espDoor struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type espCamera struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	StreamURL string `json:"stream_url"`
}

type espUISettings struct {
	Language            string `json:"language"`
	ScreensaverAfterSec int    `json:"screensaver_after_sec"`
	BrightnessIdle      int    `json:"brightness_idle"`
}

// handleESPConfig renders the snapshot for the calling ESP-Viewer.
// MAC comes from the bearer middleware's context value.
func (s *Server) handleESPConfig(w http.ResponseWriter, r *http.Request) {
	mac := ESPMACFromContext(r.Context())
	if mac == "" {
		http.Error(w, "no esp identity", http.StatusUnauthorized)
		return
	}
	info, err := s.mockMgr.GetViewerInfo(r.Context(), mac)
	if err != nil {
		if errors.Is(err, mockmanager.ErrViewerNotFound) {
			http.Error(w, "viewer not found", http.StatusNotFound)
			return
		}
		s.log.Error("esp config get viewer", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// TODO Saison 13-03+: stream-URL aus go2rtc-Config holen,
	// doors aus uaapi.ListDoors, location_name aus uaapi-Sitemap.
	// Aktuell Defaults damit das ESP-Firmware-Skelett bauen kann.
	resp := espConfigResponse{
		MieterName:   info.Name,
		LocationName: "Hauseingang",
		Stream: espStream{
			URL:         "",
			Type:        "mjpeg",
			AuthHeader:  "",
			FallbackURL: "",
		},
		Doors:   []espDoor{},
		Cameras: []espCamera{},
		UI: espUISettings{
			Language:            "de",
			ScreensaverAfterSec: 60,
			BrightnessIdle:      30,
		},
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleESPEvents holds a server-sent-events stream open for the
// calling ESP-Viewer. The stream emits:
//
//	event: heartbeat                              every ~30 seconds
//	event: doorbell.ring   data: {ev-json}        on /remote_view (S13-03)
//	event: doorbell.cancel data: {ev-json}        on UA cancel or peer-answer
//	event: config.changed                         on admin edit (later seasons)
//	event: auth.token.rotate                      on regenerate-token (later)
//
// Each viewer's events come from eventbus.Bus.Subscribe(mac); the
// publisher side belongs to the doorbell-hub / admin-edit code
// path that follows in S13-03.
func (s *Server) handleESPEvents(w http.ResponseWriter, r *http.Request) {
	mac := ESPMACFromContext(r.Context())
	if mac == "" {
		http.Error(w, "no esp identity", http.StatusUnauthorized)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	if s.eventBus == nil {
		http.Error(w, "event bus not configured", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := s.eventBus.Subscribe(mac)
	defer s.eventBus.Unsubscribe(mac, ch)

	hb := espHeartbeatInterval
	if s.eventsHeartbeat > 0 {
		hb = s.eventsHeartbeat
	}
	ticker := time.NewTicker(hb)
	defer ticker.Stop()

	// Initial heartbeat so the client confirms the stream is open.
	writeSSE(w, flusher, "heartbeat", fmt.Sprintf(`{"server_time":%d}`, time.Now().Unix()))

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			writeSSE(w, flusher, ev.Type, ev.JSON)
		case <-ticker.C:
			writeSSE(w, flusher, "heartbeat", fmt.Sprintf(`{"server_time":%d}`, time.Now().Unix()))
		}
	}
}

func writeSSE(w http.ResponseWriter, flusher http.Flusher, eventType, data string) {
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, data)
	flusher.Flush()
}

// publishToESP is a small convenience for the doorbell wire-up
// in S13-03 to push without importing eventbus directly.
func (s *Server) publishToESP(mac string, ev eventbus.Event) int {
	if s.eventBus == nil {
		return 0
	}
	return s.eventBus.Publish(mac, ev)
}

// espAnswerRequest is the JSON body of POST /esp/answer.
type espAnswerRequest struct {
	EventID string `json:"event_id"`
	Action  string `json:"action"` // "answer" | "reject"
}

// handleESPAnswer marks the active doorbell event for the calling
// ESP as answered or rejected, and pushes a doorbell.cancel to
// every other ESP-Viewer that the same UA-user owns (so phones,
// tablets, panels stop ringing).
//
// FIX4-d implements the push side and the audit trail; the
// actual audio-relay setup (WebRTC handshake, RTC offer / answer
// exchange between caller and answerer) lands in S13-03 together
// with the /remote_view wire-up.
func (s *Server) handleESPAnswer(w http.ResponseWriter, r *http.Request) {
	mac := ESPMACFromContext(r.Context())
	if mac == "" {
		http.Error(w, "no esp identity", http.StatusUnauthorized)
		return
	}
	var body espAnswerRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if body.EventID == "" {
		http.Error(w, "event_id required", http.StatusBadRequest)
		return
	}
	var reason string
	switch body.Action {
	case "answer":
		reason = "answered_elsewhere"
	case "reject":
		reason = "rejected"
	default:
		http.Error(w, "action must be 'answer' or 'reject'", http.StatusBadRequest)
		return
	}

	cancelJSON := fmt.Sprintf(`{"event_id":%q,"reason":%q}`, body.EventID, reason)
	cancelEvent := eventbus.Event{Type: "doorbell.cancel", JSON: cancelJSON}

	if body.Action == "reject" {
		// Reject schiesst auch dem antwortenden ESP selbst den
		// Cancel-Event - die UI kann das Ringing-Overlay schliessen
		// ohne lokales Branching.
		s.publishToESP(mac, cancelEvent)
	}
	siblings, err := s.mockMgr.SiblingESPMACs(r.Context(), mac)
	if err != nil {
		s.log.Error("esp answer siblings", "err", err, "mac_prefix", mac[:8])
	}
	if s.eventBus != nil && len(siblings) > 0 {
		s.eventBus.PublishAll(siblings, cancelEvent)
	}
	s.log.Info("esp answer",
		"mac_prefix", mac[:8],
		"event_id", body.EventID,
		"action", body.Action,
		"siblings_notified", len(siblings),
	)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":                true,
		"siblings_notified": len(siblings),
	})
}

// handleESPReject is the saison-13-08 dedicated reject endpoint.
// It mirrors handleMieterReject:
//   - doorbellcalls.MarkRejected (CAS, idempotent stale path)
//   - publishes a doorbell.cancel on the ESP's MAC (own SSE +
//     mieter sessions on the same MAC see it through the bus)
//   - notifyUDMReject pushes /call_admin_result to the intercom
//     so it stops ringing immediately rather than waiting on the
//     30-second hardware timeout
//
// /esp/answer with action="reject" remains available as a legacy
// shorthand; new firmware should prefer /esp/reject because it
// stops the intercom immediately.
func (s *Server) handleESPReject(w http.ResponseWriter, r *http.Request) {
	mac := ESPMACFromContext(r.Context())
	if mac == "" {
		http.Error(w, "no esp identity", http.StatusUnauthorized)
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
	if err := s.calls.MarkRejected(r.Context(), body.EventID, mac); err != nil {
		if errors.Is(err, doorbellcalls.ErrCallNotFound) {
			// Already cancelled / timed out; treat as no-op success
			// so the ESP UI doesn't paint an error toast.
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "note": "stale"})
			return
		}
		s.log.Error("esp reject mark", "err", err, "event_id", body.EventID, "mac_prefix", mac[:8])
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	cancelEvent := eventbus.Event{
		Type: "doorbell.cancel",
		JSON: fmt.Sprintf(`{"event_id":%q,"reason":%q}`,
			body.EventID, doorbellcalls.ReasonRejected),
	}
	s.publishToESP(mac, cancelEvent)
	siblings, err := s.mockMgr.SiblingESPMACs(r.Context(), mac)
	if err != nil {
		s.log.Warn("esp reject siblings", "err", err, "mac_prefix", mac[:8])
	}
	if s.eventBus != nil && len(siblings) > 0 {
		s.eventBus.PublishAll(siblings, cancelEvent)
	}
	s.notifyUDMReject(r.Context(), body.EventID, mac)
	s.log.Info("esp reject",
		"mac_prefix", mac[:8],
		"event_id", body.EventID,
		"siblings_notified", len(siblings),
	)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":                true,
		"siblings_notified": len(siblings),
	})
}

// espUnlockRequest is the JSON body of POST /esp/unlock.
type espUnlockRequest struct {
	DoorID  string `json:"door_id"`
	EventID string `json:"event_id"` // optional - manual unlock has no event
}

// handleESPUnlock relays an unlock request to the UA-API on
// behalf of the calling ESP. The ESP's MAC is included in the
// uaapi audit fields so the UA-side log shows which display
// triggered the unlock.
//
// Returns 503 if no UA-API client is configured yet (admin still
// needs to enter the base URL + token in /a/settings).
func (s *Server) handleESPUnlock(w http.ResponseWriter, r *http.Request) {
	mac := ESPMACFromContext(r.Context())
	if mac == "" {
		http.Error(w, "no esp identity", http.StatusUnauthorized)
		return
	}
	var body espUnlockRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if body.DoorID == "" {
		http.Error(w, "door_id required", http.StatusBadRequest)
		return
	}
	if s.ua == nil {
		http.Error(w, "ua-api not configured", http.StatusServiceUnavailable)
		return
	}
	info, err := s.mockMgr.GetViewerInfo(r.Context(), mac)
	if err != nil {
		s.log.Error("esp unlock viewer info", "err", err, "mac_prefix", mac[:8])
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.ua.UnlockDoor(r.Context(), body.DoorID, uaapiUnlockReq(info)); err != nil {
		s.log.Warn("esp unlock failed", "err", err, "door_id", body.DoorID, "mac_prefix", mac[:8])
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}
	s.log.Info("esp unlock",
		"mac_prefix", mac[:8],
		"door_id", body.DoorID,
		"event_id", body.EventID,
	)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// espStateRequest is the JSON body of POST /esp/state.
type espStateRequest struct {
	Screen      string `json:"screen"`        // idle|incoming|active|settings
	LastInputTS int64  `json:"last_input_ts"` // unix seconds
	UptimeSec   int64  `json:"uptime_sec"`
}

// handleESPState ingests an ESP-side status report. Saison
// 13-02-FIX4-d keeps state in memory only (per-MAC map under
// Server) - the admin dashboard tile that surfaces it lands in
// a later season and can promote the storage to a real table
// if persistence turns out to matter across server restarts.
func (s *Server) handleESPState(w http.ResponseWriter, r *http.Request) {
	mac := ESPMACFromContext(r.Context())
	if mac == "" {
		http.Error(w, "no esp identity", http.StatusUnauthorized)
		return
	}
	var body espStateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if err := s.mockMgr.TouchESPSeen(r.Context(), mac); err != nil {
		s.log.Warn("esp state touch", "err", err, "mac_prefix", mac[:8])
	}
	s.recordESPState(mac, body)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// recordESPState stores the latest snapshot under the Server's
// per-MAC map. The map is created lazily on first call; the
// callers are an HTTP handler and the dashboard reader, both
// already serialized by the http.Server's request goroutines
// for the same MAC, but multiple MACs can race so we lock.
func (s *Server) recordESPState(mac string, body espStateRequest) {
	s.espStateMu.Lock()
	defer s.espStateMu.Unlock()
	if s.espState == nil {
		s.espState = make(map[string]ESPState)
	}
	s.espState[mac] = ESPState{
		Screen:      body.Screen,
		LastInputTS: body.LastInputTS,
		UptimeSec:   body.UptimeSec,
		ReceivedAt:  time.Now(),
	}
}

// ESPState is the latest report received from one ESP-Viewer.
// Exported so dashboard handlers can read snapshots.
type ESPState struct {
	Screen      string
	LastInputTS int64
	UptimeSec   int64
	ReceivedAt  time.Time
}

// ESPState returns the most recent state report for the given
// MAC, or false if no /esp/state call has landed yet.
func (s *Server) ESPState(mac string) (ESPState, bool) {
	s.espStateMu.RLock()
	defer s.espStateMu.RUnlock()
	st, ok := s.espState[mac]
	return st, ok
}

// handleESPHeartbeat is the polling fallback for environments
// where SSE is blocked. ESPs that fail to keep /esp/events open
// hit this endpoint on a slower interval. The response is
// intentionally tiny so a battery-friendly firmware can poll
// often without burning bytes.
func (s *Server) handleESPHeartbeat(w http.ResponseWriter, r *http.Request) {
	mac := ESPMACFromContext(r.Context())
	if mac == "" {
		http.Error(w, "no esp identity", http.StatusUnauthorized)
		return
	}
	if err := s.mockMgr.TouchESPSeen(r.Context(), mac); err != nil {
		s.log.Warn("esp heartbeat touch", "err", err, "mac_prefix", mac[:8])
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":          true,
		"server_time": time.Now().Unix(),
	})
}
