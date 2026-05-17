package httpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"carvilon.local/server/internal/doorbellhub"
)

// defaultEventsHeartbeat is how often the SSE handler sends a
// keepalive comment when nothing else is happening. Reverse
// proxies (nginx default idle timeout is 60s) need to see some
// byte movement to keep the connection alive.
const defaultEventsHeartbeat = 30 * time.Second

// handleMieterEvents holds a long-lived SSE connection for one
// tenant browser. It registers a doorbellhub subscriber for the
// session's mock_mac and streams events as they arrive.
//
// The handler returns on client disconnect (r.Context().Done)
// or server shutdown. The defer cleanup() releases the hub
// subscription and closes the events channel so no goroutine
// leaks behind the listener.
//
// Saison 12-06: subscriptions are keyed by mock_mac, matching
// the new mock-centric routing model.
func (s *Server) handleMieterEvents(w http.ResponseWriter, r *http.Request) {
	mockMAC := MockMACFromContext(r.Context())
	if mockMAC == "" {
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

	sub, cleanup := s.hub.Subscribe(mockMAC)
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

// writeSSEEvent emits one SSE record per the Claude-Design
// library contract (S13-02-FIX3):
//
//	event: doorbell_start
//	data:  { "door": "Hauseingang", "ts": "2026-05-13T23:36:14Z" }
//
// The hub's richer Event struct (MockMAC, RequestID, RoomID...)
// is shrunk to the two fields the library client expects. We
// also keep the legacy fields available under nested "raw" so
// future frontends can opt in without a hub change.
//
// Saison 14-03-FIX03 Sub-2: TypeUnreadCount frames use a
// dedicated minimal payload {"count": N} so the
// screensaver badge does not have to dig through .raw.
//
// Saison 14-XX: TypeConfigChanged ships an empty `{}` payload -
// receivers refetch from the relevant config endpoint rather
// than reading any field on the event itself.
//
// The empty line at the end is required by the SSE protocol.
func writeSSEEvent(w http.ResponseWriter, ev doorbellhub.Event) error {
	var payload map[string]any
	switch ev.Type {
	case doorbellhub.TypeUnreadCount:
		payload = map[string]any{"count": ev.UnreadCount}
	case doorbellhub.TypeConfigChanged:
		payload = map[string]any{}
	default:
		payload = map[string]any{
			"door": doorNameFor(ev),
			"ts":   time.UnixMilli(ev.CreatedAt).UTC().Format(time.RFC3339),
			"raw":  ev,
		}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
	return err
}

// doorNameFor maps the doorbellhub Event onto a human door label
// for the SSE payload. The hub does not yet carry a door name
// (mock_mac is the routing key), so for now we surface the
// device MAC when present and fall back to "Hauseingang".
func doorNameFor(ev doorbellhub.Event) string {
	if ev.DeviceID != "" {
		return ev.DeviceID
	}
	return "Hauseingang"
}
