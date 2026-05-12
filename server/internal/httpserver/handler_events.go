package httpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"unifix.local/server/internal/doorbellhub"
)

// defaultEventsHeartbeat is how often the SSE handler sends a
// keepalive comment when nothing else is happening. Reverse
// proxies (nginx default idle timeout is 60s) need to see some
// byte movement to keep the connection alive.
const defaultEventsHeartbeat = 30 * time.Second

// handleMieterEvents holds a long-lived SSE connection for one
// tenant browser. It registers a doorbellhub subscriber for the
// session's ua_user_id and streams events as they arrive.
//
// The handler returns on client disconnect (r.Context().Done)
// or server shutdown. The defer cleanup() releases the hub
// subscription and closes the events channel so no goroutine
// leaks behind the listener.
func (s *Server) handleMieterEvents(w http.ResponseWriter, r *http.Request) {
	uaUserID := UAUserIDFromContext(r.Context())
	if uaUserID == "" {
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}
	if s.hub == nil {
		http.Error(w, "events not configured", http.StatusServiceUnavailable)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// nginx-spezifisch: keine Buffer-Bildung am Proxy.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	sub, cleanup := s.hub.Subscribe(uaUserID)
	defer cleanup()

	// Initial comment so the browser onopen fires immediately.
	_, _ = fmt.Fprint(w, ":connected\n\n")
	flusher.Flush()

	interval := s.eventsHeartbeat
	if interval <= 0 {
		interval = defaultEventsHeartbeat
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-sub.Events:
			if !ok {
				return
			}
			if err := writeSSEEvent(w, ev); err != nil {
				s.log.Warn("sse write failed", "err", err)
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ":keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeSSEEvent emits one SSE record. The empty line at the end
// is required by the protocol.
func writeSSEEvent(w http.ResponseWriter, ev doorbellhub.Event) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
	return err
}
