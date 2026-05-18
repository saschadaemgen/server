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
	"io"
	"net/http"
	"strings"
	"time"

	"carvilon.local/server/internal/doorbellcalls"
	"carvilon.local/server/internal/eventbus"
	"carvilon.local/server/internal/mockmanager"
	"carvilon.local/server/internal/uaapi"
	"carvilon.local/server/internal/weather"
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
	MieterName   string        `json:"mieter_name"`
	LocationName string        `json:"location_name"`
	Stream       espStream     `json:"stream"`
	Doors        []espDoor     `json:"doors"`
	Cameras      []espCamera   `json:"cameras"`
	UI           espUISettings `json:"ui"`
	// Saison 14-01b additions. IdleViewMode tells the firmware
	// which start screen to draw; Weather is a snapshot the ESP
	// can use to render its own weather card without an extra
	// /esp/weather round-trip. Both fields are safe to ignore
	// for older firmware that does not know about them.
	IdleViewMode string            `json:"idle_view_mode"`
	Weather      *weather.Snapshot `json:"weather,omitempty"`
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

// espUISettings is the ui-block returned by GET /esp/config.
//
// Saison 14-XX expands the block from the FIX4-d placeholders
// (language + screensaver_after_sec + brightness_idle, hardcoded
// defaults) to the persisted per-viewer values that POST
// /esp/settings writes. ScreensaverAfterSec is kept as an alias
// for AutoScreensaverSeconds so existing firmware that only
// reads the old key keeps working; new firmware should prefer
// the explicit names.
type espUISettings struct {
	Language               string `json:"language"`
	IdleViewMode           string `json:"idle_view_mode"`
	AutoScreensaverSeconds int    `json:"auto_screensaver_seconds"`
	ScreenOffAfterSec      int    `json:"screen_off_after_sec"`
	BrightnessIdle         int    `json:"brightness_idle"`
	// Saison 14-04-Phase2-FIX05. "vertical" / "horizontal";
	// ESP-Chat baut den Switch in scr_screensaver.c separat,
	// das Feld liefert die Praeferenz schon mit.
	ClockLayout string `json:"clock_layout"`
	// ScreensaverAfterSec is the legacy alias from FIX4-d; mirrors
	// AutoScreensaverSeconds verbatim. Will be dropped after the
	// ESP firmware migration to the canonical key lands.
	ScreensaverAfterSec int `json:"screensaver_after_sec"`
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
	autoSec := info.ResolveAutoScreensaverSeconds()
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
			Language:               info.ResolveLanguage(),
			IdleViewMode:           info.ResolveIdleViewMode(),
			AutoScreensaverSeconds: autoSec,
			ScreensaverAfterSec:    autoSec,
			ScreenOffAfterSec:      info.ResolveScreenOffAfterSec(),
			BrightnessIdle:         info.ResolveBrightnessIdle(),
			ClockLayout:            info.ResolveClockLayout(),
		},
		IdleViewMode: info.ResolveIdleViewMode(),
		Weather:      s.fetchHomeWeather(r),
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
//
// Both fields are optional. The ESP firmware has three usable
// shapes:
//
//	{}                            auto-resolve door via the
//	                              viewer's paired_intercom_mac
//	{"event_id":"<cancel_token>"} same, plus the active doorbell
//	                              call's cancel_token for audit
//	{"door_id":"<uuid>"}          explicit override (e.g. when
//	                              the firmware later supports
//	                              picking a non-paired door)
//
// Saison 14-01-FIX02: door_id is no longer required. The
// server resolves the active door via uaapi.LookupDoorForIntercom
// using the viewer's paired_intercom_mac column (the same
// auto-resolution the mieter standby path uses, S13-07).
type espUnlockRequest struct {
	DoorID  string `json:"door_id,omitempty"`
	EventID string `json:"event_id,omitempty"`
}

// handleESPUnlock relays an unlock request to the UA-API on
// behalf of the calling ESP. The ESP's MAC is included in the
// uaapi audit fields so the UA-side log shows which display
// triggered the unlock.
//
// Returns 503 if no UA-API client is configured yet (admin still
// needs to enter the base URL + token in /a/settings).
//
// Saison 14-01-FIX02: door_id auto-resolution.
//   - door_id explicit in body          -> use it (door_source=body)
//   - door_id empty + paired_intercom   -> uaapi lookup (door_source=auto)
//   - door_id empty + no paired_intercom -> 400 "no paired intercom configured"
//   - door_id empty + intercom unbound   -> 400 "paired intercom not assigned..."
func (s *Server) handleESPUnlock(w http.ResponseWriter, r *http.Request) {
	mac := ESPMACFromContext(r.Context())
	if mac == "" {
		http.Error(w, "no esp identity", http.StatusUnauthorized)
		return
	}
	// Tolerant body decode: empty body, "{}" and partial JSON all
	// land on the auto-resolution branch via DoorID == "". Only a
	// completely garbled body shape (an array, a string, etc.)
	// short-circuits with 400 so we never silently ignore a
	// malformed request.
	var body espUnlockRequest
	if r.ContentLength != 0 {
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&body); err != nil {
			// io.EOF on empty bodies is fine; everything else is
			// a real syntax error worth reporting.
			if !errors.Is(err, io.EOF) {
				s.log.Warn("esp unlock: invalid json",
					"viewer_mac", mac, "err", err)
				http.Error(w, "invalid json", http.StatusBadRequest)
				return
			}
		}
	}

	if s.ua == nil {
		http.Error(w, "ua-api not configured", http.StatusServiceUnavailable)
		return
	}
	info, err := s.mockMgr.GetViewerInfo(r.Context(), mac)
	if err != nil {
		s.log.Error("esp unlock: get viewer failed",
			"viewer_mac", mac, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	doorID := strings.TrimSpace(body.DoorID)
	doorSource := "body"
	if doorID == "" {
		doorSource = "auto"
		paired := strings.TrimSpace(info.PairedIntercomMAC)
		if paired == "" {
			s.log.Warn("esp unlock: no paired intercom",
				"viewer_mac", mac, "event_id", body.EventID)
			http.Error(w, "no paired intercom configured", http.StatusBadRequest)
			return
		}
		resolved, err := s.ua.LookupDoorForIntercom(r.Context(), paired)
		if err != nil {
			s.log.Error("esp unlock: uaapi error",
				"viewer_mac", mac, "paired_intercom", paired,
				"event_id", body.EventID, "err", err)
			http.Error(w, "ua-api door lookup failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		if resolved == "" {
			s.log.Warn("esp unlock: paired intercom not assigned",
				"viewer_mac", mac, "paired_intercom", paired,
				"event_id", body.EventID)
			http.Error(w, "paired intercom not assigned to any door", http.StatusBadRequest)
			return
		}
		doorID = resolved
	}

	s.log.Info("esp unlock",
		"viewer_mac", mac,
		"door_id", doorID,
		"door_source", doorSource,
		"event_id", body.EventID,
	)
	if err := s.ua.UnlockDoor(r.Context(), doorID, uaapiUnlockReq(info)); err != nil {
		s.log.Warn("esp unlock: uaapi unlock failed",
			"viewer_mac", mac, "door_id", doorID, "err", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":          true,
		"door_id":     doorID,
		"door_source": doorSource,
	})
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
