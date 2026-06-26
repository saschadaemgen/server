package httpserver

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// handleAdminMQTTWSInfo tells the in-editor MQTT console how to reach
// the broker's WebSocket listener: the ws(s):// URL, derived from the
// admin request's scheme (wss when the admin page is HTTPS, to avoid a
// mixed-content block) and host, plus the configured WS port. The
// console prefills its connect form from this. Returns enabled:false
// when the broker is down or the WS listener is off.
func (s *Server) handleAdminMQTTWSInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	resp := map[string]any{"enabled": false}
	if s.mqtt != nil {
		st := s.mqtt.Status()
		set := s.mqtt.SettingsSnapshot()
		if st.Running && set.WSEnabled {
			scheme := "ws"
			if st.WSSecure {
				scheme = "wss"
			}
			host := r.Host
			if h, _, err := net.SplitHostPort(r.Host); err == nil {
				host = h
			}
			resp = map[string]any{
				"enabled": true,
				"secure":  st.WSSecure,
				"url":     fmt.Sprintf("%s://%s:%d", scheme, host, set.WSPort),
			}
		}
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// handleAdminMQTTMonitor streams live broker activity (connects,
// disconnects, publishes, subscribes) to the designer's MQTT console
// over SSE, mirroring the engine monitor: a "backlog" snapshot of
// recent events, then one "event" per new activity. A periodic
// comment heartbeat keeps idle connections alive through proxies.
func (s *Server) handleAdminMQTTMonitor(w http.ResponseWriter, r *http.Request) {
	if s.mqtt == nil {
		http.Error(w, "mqtt broker not available", http.StatusServiceUnavailable)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	console := s.mqtt.Console()
	events, cancel := console.Subscribe(128)
	defer cancel()

	if err := writeMQTTSSE(w, "backlog", map[string]any{"events": console.Backlog()}); err != nil {
		return
	}
	flusher.Flush()

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-events:
			if !ok {
				return
			}
			if err := writeMQTTSSE(w, "event", ev); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeMQTTSSE(w http.ResponseWriter, event string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	return err
}
