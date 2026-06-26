package mqttdriver

import (
	"context"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	"carvilon.local/server/internal/db"
	"carvilon.local/server/internal/engine"
	"carvilon.local/server/internal/mqttbroker"
	"carvilon.local/server/internal/mqttstore"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startBroker brings up a real embedded broker and returns its inline
// client (what the mqtt: driver binds to in production).
func startBroker(t *testing.T) mqttbroker.InlineClient {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	store := mqttstore.New(d.DB, func(context.Context) (string, error) { return "pep", nil })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := mqttbroker.New(store, mqttbroker.NewConsole(50), log, t.TempDir(), mqttbroker.Settings{
		Enabled: true, LANHost: "127.0.0.1", TCPPort: freePort(t), TLSHost: "127.0.0.1", TLSPort: freePort(t),
	})
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("broker Start: %v", err)
	}
	t.Cleanup(m.Stop)
	time.Sleep(80 * time.Millisecond)
	cli, ok := m.Inline()
	if !ok {
		t.Fatal("broker inline client not available after Start")
	}
	return cli
}

func typesFor(kind engine.Kind) (src, sink string) {
	switch kind {
	case engine.Bool:
		return engine.TypeSourceChannel, engine.TypeSinkChannel
	case engine.Float:
		return engine.TypeSourceChannelFloat, engine.TypeSinkChannelFloat
	default:
		return engine.TypeSourceChannelText, engine.TypeSinkChannelText
	}
}

// TestMQTTDriver_EndToEnd drives the full step-2 chain per value kind:
// an external publish on the In topic flows through a source -> sink
// passthrough graph and is re-published on the Out topic. This proves
// the source parses inbound payloads, feeds them deterministically via
// EnqueueInput, and the sink formats and publishes outbound values.
func TestMQTTDriver_EndToEnd(t *testing.T) {
	cli := startBroker(t)

	type step struct{ publish, want string }
	cases := []struct {
		name  string
		kind  engine.Kind
		steps []step
	}{
		{"float", engine.Float, []step{{"23.5", "23.5"}}},
		// bool: the engine only propagates on CHANGE, and the source's
		// initial value is false, so test the rising edge then the falling
		// edge (a same-as-initial "off" alone would correctly emit nothing).
		{"bool", engine.Bool, []step{{"on", "true"}, {"off", "false"}}},
		{"text", engine.Text, []step{{"hello world", "hello world"}}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			inTopic := "test/" + c.name + "/in"
			outTopic := "test/" + c.name + "/out"
			srcType, sinkType := typesFor(c.kind)

			g := engine.Graph{
				Schema: 1,
				Nodes: []engine.GraphNode{
					{ID: "in", Type: srcType, Params: map[string]any{"channel": "mqtt:" + inTopic}},
					{ID: "out", Type: sinkType, Params: map[string]any{"channel": "mqtt:" + outTopic}},
				},
				Edges: []engine.GraphEdge{{From: "in:out", To: "out:in"}},
			}
			eng, err := engine.Build(g, engine.DefaultRegistry(), 50*time.Millisecond)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}

			drv := NewDriver(cli, []engine.Channel{
				{Address: inTopic, Label: inTopic, Kind: c.kind},
				{Address: outTopic, Label: outTopic, Kind: c.kind},
			}, nil)
			t.Cleanup(func() { drv.Close() })

			table := engine.BindingTable{
				"mqtt:" + inTopic:  {Prefix: engine.PrefixMQTT, Addr: inTopic},
				"mqtt:" + outTopic: {Prefix: engine.PrefixMQTT, Addr: outTopic},
			}
			reg := engine.NewDriverRegistry()
			reg.RegisterSource(engine.PrefixMQTT, drv)
			reg.RegisterSink(engine.PrefixMQTT, drv)
			if err := engine.BindGraph(eng, g, table, nil, reg); err != nil {
				t.Fatalf("BindGraph: %v", err)
			}

			// Observe the Out topic with a second inline subscription.
			out := make(chan string, 8)
			if err := cli.Subscribe(outTopic, 9999, func(_ string, payload []byte) {
				out <- string(payload)
			}); err != nil {
				t.Fatalf("observe subscribe: %v", err)
			}
			t.Cleanup(func() { _ = cli.Unsubscribe(outTopic, 9999) })

			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			for _, st := range c.steps {
				// Inbound publish (as an external device would).
				if err := cli.Publish(inTopic, []byte(st.publish), false, 0); err != nil {
					t.Fatalf("inbound publish: %v", err)
				}
				deadline := time.After(3 * time.Second)
				for done := false; !done; {
					select {
					case got := <-out:
						if got != st.want {
							t.Fatalf("step %q: Out payload = %q, want %q", st.publish, got, st.want)
						}
						done = true
					case <-ticker.C:
						eng.Tick()
					case <-deadline:
						t.Fatalf("step %q: no Out message within timeout", st.publish)
					}
				}
			}
		})
	}
}
