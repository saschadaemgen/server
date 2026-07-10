package mqttdriver

import (
	"testing"
	"time"

	"carvilon.local/server/internal/engine"
)

// TestMQTTDriver_Gen1OnOffSink proves the Gen1 relay-command chain the
// editor module's binding generator emits: a bool sink with the
// payload:"on-off" ChannelConfig publishes the raw "on"/"off" strings a
// Gen1 command topic expects (never "true"/"false"), and the flat Gen1
// state topic ("shellies/<id>/relay/0", payload on/off) feeds a bool
// source without a selector.
func TestMQTTDriver_Gen1OnOffSink(t *testing.T) {
	cli := startBroker(t)
	const stateTopic = "shellies/shelly-abc/relay/0"
	const cmdTopic = "shellies/shelly-abc/relay/0/command"

	g := engine.Graph{
		Schema: 1,
		Nodes: []engine.GraphNode{
			{ID: "in", Type: engine.TypeSourceChannel, Params: map[string]any{"channel": "mqtt:" + stateTopic}},
			{ID: "out", Type: engine.TypeSinkChannel, Params: map[string]any{"channel": "mqtt:" + cmdTopic, "payload": "on-off"}},
		},
		Edges: []engine.GraphEdge{{From: "in:out", To: "out:in"}},
	}
	eng, err := engine.Build(g, engine.DefaultRegistry(), 20*time.Millisecond)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	drv := NewDriver(cli, []engine.Channel{
		{Address: stateTopic, Label: stateTopic, Kind: engine.Bool},
		{Address: cmdTopic, Label: cmdTopic, Kind: engine.Bool},
	}, nil)
	t.Cleanup(func() { drv.Close() })
	reg := engine.NewDriverRegistry()
	reg.RegisterSource(engine.PrefixMQTT, drv)
	reg.RegisterSink(engine.PrefixMQTT, drv)
	table := engine.BindingTable{
		"mqtt:" + stateTopic: {Prefix: engine.PrefixMQTT, Addr: stateTopic},
		"mqtt:" + cmdTopic:   {Prefix: engine.PrefixMQTT, Addr: cmdTopic},
	}
	configs := map[string]engine.ChannelConfig{
		"mqtt:" + cmdTopic: {"payload": "on-off"},
	}
	if err := engine.BindGraph(eng, g, table, configs, reg); err != nil {
		t.Fatalf("BindGraph: %v", err)
	}

	cmds := make(chan []byte, 8)
	if err := cli.Subscribe(cmdTopic, 9995, func(_ string, p []byte) {
		b := make([]byte, len(p))
		copy(b, p)
		cmds <- b
	}); err != nil {
		t.Fatalf("observe subscribe: %v", err)
	}
	t.Cleanup(func() { _ = cli.Unsubscribe(cmdTopic, 9995) })

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	await := func(want string) {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case raw := <-cmds:
				if got := string(raw); got != want {
					t.Fatalf("command payload = %q, want %q", got, want)
				}
				return
			case <-ticker.C:
				eng.Tick()
			case <-deadline:
				t.Fatalf("no %q command within timeout", want)
			}
		}
	}
	// The device's own state grammar drives the source; the sink must
	// answer in the same grammar.
	if err := cli.Publish(stateTopic, []byte("on"), false, 0); err != nil {
		t.Fatalf("publish on: %v", err)
	}
	await("on")
	if err := cli.Publish(stateTopic, []byte("off"), false, 0); err != nil {
		t.Fatalf("publish off: %v", err)
	}
	await("off")
}

// TestConfigureOutputPayloadStyle pins the bind-time contract: unknown
// styles fail loudly, on-off demands a bool channel, and an RPC sink
// refuses the plain-publish style (its grammar is the JSON envelope).
func TestConfigureOutputPayloadStyle(t *testing.T) {
	drv := NewDriver(nopInline{}, []engine.Channel{
		{Address: "t/bool", Kind: engine.Bool},
		{Address: "t/float", Kind: engine.Float},
		{Address: "t/rpc#Switch.Set:0", Kind: engine.Bool},
	}, nil)
	t.Cleanup(func() { drv.Close() })
	if err := drv.ConfigureOutput("t/bool", engine.ChannelConfig{"payload": "on-off"}); err != nil {
		t.Fatalf("on-off on bool: %v", err)
	}
	if err := drv.ConfigureOutput("t/float", engine.ChannelConfig{"payload": "on-off"}); err == nil {
		t.Fatal("on-off on float must fail the bind")
	}
	if err := drv.ConfigureOutput("t/rpc#Switch.Set:0", engine.ChannelConfig{"payload": "on-off"}); err == nil {
		t.Fatal("on-off on an rpc sink must fail the bind")
	}
	if err := drv.ConfigureOutput("t/bool", engine.ChannelConfig{"payload": "garbage"}); err == nil {
		t.Fatal("unknown payload style must fail the bind")
	}
	if err := drv.ConfigureOutput("t/bool", engine.ChannelConfig{}); err != nil {
		t.Fatalf("default style: %v", err)
	}
}

// nopInline satisfies the inline-client seam for bind-time-only tests.
type nopInline struct{}

func (nopInline) Publish(string, []byte, bool, byte) error          { return nil }
func (nopInline) Subscribe(string, int, func(string, []byte)) error { return nil }
func (nopInline) Unsubscribe(string, int) error                     { return nil }
