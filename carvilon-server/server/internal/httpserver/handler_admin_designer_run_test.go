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

// TestBuildBindingTable: I/O nodes' channel params (physical refs) map
// through to PhysicalAddr; non-I/O nodes are skipped; a malformed ref
// errors.
func TestBuildBindingTable(t *testing.T) {
	g := engine.Graph{Nodes: []engine.GraphNode{
		{ID: "btn", Type: "input.manual"}, // not an I/O node -> skipped
		{ID: "in", Type: engine.TypeSourceChannel, Params: map[string]any{"channel": "gpio:gpiochip0:17"}},
		{ID: "out", Type: engine.TypeSinkChannel, Params: map[string]any{"channel": "virtual:lamp0"}},
	}}
	table, err := buildBindingTable(g)
	if err != nil {
		t.Fatalf("buildBindingTable: %v", err)
	}
	if len(table) != 2 {
		t.Fatalf("table has %d entries, want 2: %+v", len(table), table)
	}
	if pa := table["gpio:gpiochip0:17"]; pa.Prefix != "gpio" || pa.Addr != "gpiochip0:17" {
		t.Errorf("gpio binding = %+v, want {gpio, gpiochip0:17}", pa)
	}
	if pa := table["virtual:lamp0"]; pa.Prefix != "virtual" || pa.Addr != "lamp0" {
		t.Errorf("virtual binding = %+v, want {virtual, lamp0}", pa)
	}

	if _, err := buildBindingTable(engine.Graph{Nodes: []engine.GraphNode{
		{ID: "in", Type: engine.TypeSourceChannel, Params: map[string]any{"channel": "noseparator"}},
	}}); err == nil {
		t.Errorf("a channel ref without a prefix:addr separator must error")
	}

	if table, err := buildBindingTable(engine.Graph{Nodes: []engine.GraphNode{{ID: "x", Type: "logic.or"}}}); err != nil || len(table) != 0 {
		t.Errorf("non-IO graph: table=%v err=%v, want empty/nil", table, err)
	}

	// the same physical line bound by two nodes (here an input and an
	// output) must be rejected, not silently collide on the hardware.
	if _, err := buildBindingTable(engine.Graph{Nodes: []engine.GraphNode{
		{ID: "in", Type: engine.TypeSourceChannel, Params: map[string]any{"channel": "gpio:gpiochip0:5"}},
		{ID: "out", Type: engine.TypeSinkChannel, Params: map[string]any{"channel": "gpio:gpiochip0:5"}},
	}}); err == nil {
		t.Errorf("one physical line bound by two nodes must error")
	}
}

// TestDesignerRun_GPIONodeWithoutDriver: on a host with no GPIO, a graph
// containing a GPIO node fails to bind (no gpio driver registered) and the
// run does not start - the dev-machine guard from the briefing.
func TestDesignerRun_GPIONodeWithoutDriver(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	graph := `{"schema":1,
	  "nodes":[
	    {"id":"in","type":"source.channel","params":{"channel":"gpio:gpiochip0:17"}},
	    {"id":"lamp","type":"output.lamp"}
	  ],
	  "edges":[{"from":"in:out","to":"lamp:set"}]}`
	resp, err := env.client.Post(env.ts.URL+"/a/designer/run", "application/json", strings.NewReader(graph))
	if err != nil {
		t.Fatalf("POST run: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (no gpio driver on this host)", resp.StatusCode)
	}
	if env.srv.designerRuns.get(adminTestUser) != nil {
		t.Errorf("a GPIO graph must not start a run without a gpio driver")
	}
}
