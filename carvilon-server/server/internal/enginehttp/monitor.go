// Package enginehttp exposes the engine's monitor fan-out over HTTP.
// It is kept separate from the engine core so that engine imports no
// net/http: the kernel stays pure and the transport lives here.
//
// MonitorHandler streams Server-Sent Events: one "snapshot" event with
// the current value on every wire, then one "tick" event per changed
// tick. This is what the (later) logic editor consumes to render live
// signal values flowing over the connections - the Loxone monitor
// effect. ENGINE-S1-02 verifies it with curl; no editor yet.
package enginehttp

import (
	"encoding/json"
	"fmt"
	"net/http"

	"carvilon.local/server/internal/engine"
)

// monitorBuffer is the per-client frame buffer. A client that cannot
// keep up drops frames (the engine never blocks) and resynchronises
// from the next snapshot on reconnect.
const monitorBuffer = 64

// snapshotPayload is the body of the initial "snapshot" SSE event.
type snapshotPayload struct {
	Changes []engine.Change `json:"changes"`
}

// MonitorHandler streams the engine's monitor frames as SSE. It sends
// a snapshot of the present state on connect, then forwards every
// subsequent frame until the client disconnects.
func MonitorHandler(e *engine.Engine) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		// Subscribe before snapshotting so no frame can slip through the
		// gap: a tick landing between the two is merely replayed (applying
		// the same value twice is idempotent), never lost.
		frames, cancel := e.Subscribe(monitorBuffer)
		defer cancel()

		if err := writeEvent(w, "snapshot", snapshotPayload{Changes: e.Snapshot()}); err != nil {
			return
		}
		flusher.Flush()

		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case f, ok := <-frames:
				if !ok {
					return
				}
				if err := writeEvent(w, "tick", f); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	})
}

// writeEvent serializes one SSE event: an "event:" line, a "data:"
// line carrying the JSON payload, and the terminating blank line.
func writeEvent(w http.ResponseWriter, event string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	return err
}
