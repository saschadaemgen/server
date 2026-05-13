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
//	POST /esp/unlock        relay an UA-API door unlock
//	POST /esp/state         ESP-side status report
package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"unifix.local/server/internal/eventbus"
	"unifix.local/server/internal/mockmanager"
)

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
