package httpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"carvilon.local/server/internal/engine"
)

// designerDemoGraph is the canonical staircase graph the editor emits:
// button -> staircase(3s) -> lamp, with engine port names.
const designerDemoGraph = `{"schema":1,
  "nodes":[
    {"id":"btn","type":"input.manual"},
    {"id":"stair","type":"time.staircase","params":{"duration":3}},
    {"id":"lamp","type":"output.lamp"}
  ],
  "edges":[
    {"from":"btn:out","to":"stair:trig"},
    {"from":"stair:q","to":"lamp:set"}
  ]}`

func TestDesignerRun_RequiresSession(t *testing.T) {
	env := newTestServer(t)
	resp, err := env.client.Post(env.ts.URL+"/a/designer/run", "application/json", strings.NewReader(designerDemoGraph))
	if err != nil {
		t.Fatalf("POST run: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303 (redirect to login)", resp.StatusCode)
	}
}

// TestDesignerRun_InvalidGraphReturnsIssues: a graph with an unknown type
// must come back as 400 + issues and must NOT start a run.
func TestDesignerRun_InvalidGraphReturnsIssues(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	bad := `{"schema":1,"nodes":[{"id":"x","type":"does.not.exist"}],"edges":[]}`
	resp, err := env.client.Post(env.ts.URL+"/a/designer/run", "application/json", strings.NewReader(bad))
	if err != nil {
		t.Fatalf("POST run: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var out struct {
		OK     bool           `json:"ok"`
		Issues []engine.Issue `json:"issues"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.OK || len(out.Issues) == 0 {
		t.Errorf("want ok=false with issues, got ok=%v issues=%d", out.OK, len(out.Issues))
	}
	if env.srv.designerRuns.get(adminTestUser) != nil {
		t.Errorf("an invalid graph must not start a run")
	}
}

// TestDesignerRun_DemoGraphStreamsLamp drives the full path: run the demo
// graph, open the monitor SSE, press the button via the input endpoint,
// and confirm a real engine frame reports lamp:set = true.
func TestDesignerRun_DemoGraphStreamsLamp(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	resp, err := env.client.Post(env.ts.URL+"/a/designer/run", "application/json", strings.NewReader(designerDemoGraph))
	if err != nil {
		t.Fatalf("POST run: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("run status = %d body=%s", resp.StatusCode, body)
	}
	t.Cleanup(func() {
		r, _ := env.client.Post(env.ts.URL+"/a/designer/run/stop", "application/json", nil)
		if r != nil {
			r.Body.Close()
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, env.ts.URL+"/a/designer/run/monitor", nil)
	streamClient := &http.Client{Jar: env.client.Jar}
	sse, err := streamClient.Do(req)
	if err != nil {
		t.Fatalf("open sse: %v", err)
	}
	defer sse.Body.Close()
	if sse.StatusCode != http.StatusOK {
		t.Fatalf("sse status = %d, want 200", sse.StatusCode)
	}
	br := bufio.NewReader(sse.Body)

	if ev, _ := nextSSEEvent(t, br, 2*time.Second); ev != "snapshot" {
		t.Fatalf("first event = %q, want snapshot", ev)
	}

	// Press the button → injects into the running engine.
	pr, err := env.client.Post(env.ts.URL+"/a/designer/run/input", "application/json",
		strings.NewReader(`{"node":"btn","port":"out","value":true}`))
	if err != nil {
		t.Fatalf("POST input: %v", err)
	}
	pr.Body.Close()
	if pr.StatusCode != http.StatusNoContent {
		t.Fatalf("input status = %d, want 204", pr.StatusCode)
	}

	// The very next changed tick settles button->staircase->lamp in one
	// pass, so its frame carries lamp:set = true.
	ev, data := nextSSEEvent(t, br, 2*time.Second)
	if ev != "tick" {
		t.Fatalf("event after press = %q, want tick", ev)
	}
	var f engine.Frame
	if err := json.Unmarshal([]byte(data), &f); err != nil {
		t.Fatalf("decode frame %q: %v", data, err)
	}
	sawLamp := false
	for _, c := range f.Changes {
		if c.Node == "lamp" && c.Port == "set" && c.Value.B {
			sawLamp = true
		}
	}
	if !sawLamp {
		t.Errorf("frame after press did not light the lamp: %+v", f.Changes)
	}
}
