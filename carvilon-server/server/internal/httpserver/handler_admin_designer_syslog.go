package httpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// handleDesignerSysLog streams the server's recent structured log
// entries (the logbuf ring main tees around the stdout handler) to the
// designer's System Log tab over SSE, mirroring the MQTT console
// monitor: a "backlog" snapshot of the retained entries, then one
// "entry" per new log line. A periodic comment heartbeat keeps idle
// connections alive through proxies. Route: GET /a/designer/syslog
// (requireAdminSession). 503 when main did not wire a buffer.
func (s *Server) handleDesignerSysLog(w http.ResponseWriter, r *http.Request) {
	if s.logBuf == nil {
		http.Error(w, "system log buffer not available", http.StatusServiceUnavailable)
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

	entries, cancel := s.logBuf.Subscribe(128)
	defer cancel()

	if err := writeSysLogSSE(w, "backlog", map[string]any{"entries": s.logBuf.Backlog()}); err != nil {
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
		case e, ok := <-entries:
			if !ok {
				return
			}
			if err := writeSysLogSSE(w, "entry", e); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeSysLogSSE(w http.ResponseWriter, event string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	return err
}
