package httpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

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
