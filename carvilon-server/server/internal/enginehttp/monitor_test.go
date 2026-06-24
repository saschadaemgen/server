package enginehttp

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"carvilon.local/server/internal/engine"
)

// TestMonitorHandler drives the SSE handler end-to-end over httptest:
// connect, read the snapshot event, produce a frame by ticking the
// engine, and read it back as a "tick" event.
func TestMonitorHandler(t *testing.T) {
	eng := engine.New(100 * time.Millisecond)
	mustAdd(t, eng, "btn", "input.manual", nil)
	mustAdd(t, eng, "stair", "time.staircase", map[string]engine.Value{"duration": engine.FloatVal(3)})
	mustAdd(t, eng, "lamp", "output.lamp", nil)
	eng.Connect("btn", "out", "stair", "trig")
	eng.Connect("stair", "q", "lamp", "set")

	srv := httptest.NewServer(MonitorHandler(eng))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q; want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q; want no-cache", cc)
	}

	sc := bufio.NewScanner(resp.Body)

	// 1) First event must be the snapshot, carrying lamp:set=false.
	ev := readEvent(t, sc)
	if ev.name != "snapshot" {
		t.Fatalf("first event = %q; want snapshot", ev.name)
	}
	var snap struct {
		Changes []engine.Change `json:"changes"`
	}
	if err := json.Unmarshal([]byte(ev.data), &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if !hasChange(snap.Changes, "lamp", "set", false) {
		t.Errorf("snapshot missing lamp:set=false; got %+v", snap.Changes)
	}

	// Receiving the snapshot proves the subscription is live, so frames
	// produced now are guaranteed to reach us. Press, then tick.
	eng.SetInput("btn", "out", engine.BoolVal(true))
	eng.Tick()

	// 2) Next event is the tick frame with lamp:set turning true.
	ev = readEvent(t, sc)
	if ev.name != "tick" {
		t.Fatalf("second event = %q; want tick", ev.name)
	}
	var frame engine.Frame
	if err := json.Unmarshal([]byte(ev.data), &frame); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if !hasChange(frame.Changes, "lamp", "set", true) {
		t.Errorf("tick frame missing lamp:set=true; got %+v", frame.Changes)
	}
}

type sseEvent struct {
	name string
	data string
}

// readEvent reads one full SSE event (its event: and data: lines up to
// the terminating blank line) from the scanner.
func readEvent(t *testing.T, sc *bufio.Scanner) sseEvent {
	t.Helper()
	var ev sseEvent
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if ev.name != "" || ev.data != "" {
				return ev
			} // skip leading blank lines
		case strings.HasPrefix(line, "event: "):
			ev.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			ev.data = strings.TrimPrefix(line, "data: ")
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan SSE: %v", err)
	}
	t.Fatalf("stream ended before a full event arrived")
	return ev
}

func hasChange(cs []engine.Change, node, port string, on bool) bool {
	for _, c := range cs {
		if c.Node == node && c.Port == port && c.Value.B == on {
			return true
		}
	}
	return false
}

func mustAdd(t *testing.T, e *engine.Engine, id, typ string, params map[string]engine.Value) {
	t.Helper()
	if _, err := e.AddType(id, typ, params); err != nil {
		t.Fatalf("add %s: %v", id, err)
	}
}
