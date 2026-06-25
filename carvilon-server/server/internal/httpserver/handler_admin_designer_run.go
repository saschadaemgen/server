package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"carvilon.local/server/internal/engine"
)

// designerRunTick is the wall-clock period the editor's live run advances
// the logical clock by, matching engine-monitor-demo (100ms ticks).
const designerRunTick = 100 * time.Millisecond

// designerRun is one live engine run bound to an admin session: a built
// engine driven by a wall-clock ticker. done stops both the ticker and
// the monitor SSE; closing it is idempotent.
type designerRun struct {
	eng  *engine.Engine
	done chan struct{}
	once sync.Once
}

func (r *designerRun) stop() { r.once.Do(func() { close(r.done) }) }

// loop drives the engine on the wall clock until the run is stopped. The
// editor injects input out-of-band (SetInput) between ticks; each tick
// then settles the graph and fans a Frame out to the monitor SSE.
func (r *designerRun) loop(tick time.Duration) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-t.C:
			r.eng.Tick()
		}
	}
}

// setInput injects a value, recovering from the engine's panic on an
// unknown or non-source node so a malformed request can't crash the
// server. Returns false when the node does not accept external input.
func (r *designerRun) setInput(node, port string, v engine.Value) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	r.eng.SetInput(node, port, v)
	return true
}

// designerRunSet holds at most one live run per admin user. Starting a
// new run replaces (and stops) the user's previous one.
type designerRunSet struct {
	mu     sync.Mutex
	byUser map[string]*designerRun
}

func newDesignerRunSet() *designerRunSet {
	return &designerRunSet{byUser: map[string]*designerRun{}}
}

func (s *designerRunSet) start(user string, eng *engine.Engine, tick time.Duration) *designerRun {
	run := &designerRun{eng: eng, done: make(chan struct{})}
	s.mu.Lock()
	old := s.byUser[user]
	s.byUser[user] = run
	s.mu.Unlock()
	if old != nil {
		old.stop()
	}
	go run.loop(tick)
	return run
}

func (s *designerRunSet) get(user string) *designerRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byUser[user]
}

// stopUser stops and forgets the user's current run (explicit Stop).
func (s *designerRunSet) stopUser(user string) {
	s.mu.Lock()
	run := s.byUser[user]
	delete(s.byUser, user)
	s.mu.Unlock()
	if run != nil {
		run.stop()
	}
}

// stopIfCurrent tears down a specific run on monitor disconnect, but only
// unmaps it while it is still the active one (so a reconnect that already
// started a newer run is left intact). The run is stopped regardless —
// stopping an already-replaced run is a harmless idempotent no-op.
func (s *designerRunSet) stopIfCurrent(user string, run *designerRun) {
	s.mu.Lock()
	if s.byUser[user] == run {
		delete(s.byUser, user)
	}
	s.mu.Unlock()
	run.stop()
}

// handleDesignerRun validates+builds the posted canonical graph and, on
// success, starts a live run for the admin user. Validation errors come
// back as issues with HTTP 400 and nothing is executed.
func (s *Server) handleDesignerRun(w http.ResponseWriter, r *http.Request) {
	user := AdminUserFromContext(r.Context())
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		designerJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "could not read request body"})
		return
	}
	g, err := engine.ParseGraph(body)
	if err != nil {
		designerJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid graph JSON"})
		return
	}
	eng, err := engine.Build(g, engine.DefaultRegistry(), designerRunTick)
	if err != nil {
		var ve *engine.ValidationError
		if errors.As(err, &ve) {
			designerJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "issues": ve.Issues})
			return
		}
		designerJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.designerRuns.start(user, eng, designerRunTick)
	designerJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDesignerRunMonitor streams the user's running engine as SSE: a
// "snapshot" event with the present value on every wire, then a "tick"
// event per changed tick (engine.Frame). It reuses the engine's
// Subscribe/Snapshot fan-out and tears the run down when the client
// disconnects (briefing: stop on disconnect).
func (s *Server) handleDesignerRunMonitor(w http.ResponseWriter, r *http.Request) {
	user := AdminUserFromContext(r.Context())
	run := s.designerRuns.get(user)
	if run == nil {
		http.Error(w, "no running graph", http.StatusNotFound)
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

	frames, cancel := run.eng.Subscribe(64)
	defer cancel()
	defer s.designerRuns.stopIfCurrent(user, run)

	if err := writeDesignerSSE(w, "snapshot", map[string]any{"changes": run.eng.Snapshot()}); err != nil {
		return
	}
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-run.done:
			return
		case f, ok := <-frames:
			if !ok {
				return
			}
			if err := writeDesignerSSE(w, "tick", f); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleDesignerRunInput injects the editor's button press into the
// running engine (SetInput on the input.manual node), driving real
// evaluation. A press is a true/false pulse the editor sends.
func (s *Server) handleDesignerRunInput(w http.ResponseWriter, r *http.Request) {
	user := AdminUserFromContext(r.Context())
	run := s.designerRuns.get(user)
	if run == nil {
		http.Error(w, "no running graph", http.StatusNotFound)
		return
	}
	var in struct {
		Node  string `json:"node"`
		Port  string `json:"port"`
		Value bool   `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil {
		http.Error(w, "bad input body", http.StatusBadRequest)
		return
	}
	if in.Node == "" || in.Port == "" {
		http.Error(w, "node and port are required", http.StatusBadRequest)
		return
	}
	if !run.setInput(in.Node, in.Port, engine.BoolVal(in.Value)) {
		http.Error(w, "node does not accept input", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDesignerRunStop stops the user's run (idempotent).
func (s *Server) handleDesignerRunStop(w http.ResponseWriter, r *http.Request) {
	user := AdminUserFromContext(r.Context())
	s.designerRuns.stopUser(user)
	w.WriteHeader(http.StatusNoContent)
}

func designerJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeDesignerSSE serializes one SSE event (event + data + blank line),
// mirroring enginehttp's writer (kept local so this package needs no
// access to the engine transport's unexported helper).
func writeDesignerSSE(w http.ResponseWriter, event string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	return err
}
