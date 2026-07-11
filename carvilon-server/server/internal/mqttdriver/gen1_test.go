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
	// json:<key> wraps the value; a bad key or an rpc target is refused.
	if err := drv.ConfigureOutput("t/float", engine.ChannelConfig{"payload": "json:gain"}); err != nil {
		t.Fatalf("json:gain on float: %v", err)
	}
	if err := drv.ConfigureOutput("t/rpc#Switch.Set:0", engine.ChannelConfig{"payload": "json:gain"}); err == nil {
		t.Fatal("json wrap on an rpc sink must fail the bind")
	}
	if err := drv.ConfigureOutput("t/float", engine.ChannelConfig{"payload": "json:bad key"}); err == nil {
		t.Fatal("an unsafe json wrap key must fail the bind")
	}
}

// TestValidJSONWrapKey guards the key allowlist directly.
func TestValidJSONWrapKey(t *testing.T) {
	for _, ok := range []string{"gain", "red", "turn", "white", "auto_on", "g0"} {
		if !validJSONWrapKey(ok) {
			t.Errorf("valid key %q rejected", ok)
		}
	}
	for _, bad := range []string{"", "0gain", "Gain", "a b", `a"b`, "a}b", "a:b"} {
		if validJSONWrapKey(bad) {
			t.Errorf("unsafe key %q accepted", bad)
		}
	}
}

// TestMQTTDriver_Gen1LightSinks proves the RGBW2 light bindings end to
// end on a real embedded broker: the on/off control sink publishes raw
// "on"/"off" to the command topic, and the gain sink publishes a
// well-formed JSON object ({"gain":<n>}) to the /set topic - the exact
// grammar a Gen1 light expects.
func TestMQTTDriver_Gen1LightSinks(t *testing.T) {
	cli := startBroker(t)
	const cmdTopic = "shellies/shelly-abc/color/0/command"
	const setTopic = "shellies/shelly-abc/color/0/set"
	const stateTopic = "shellies/shelly-abc/color/0"

	g := engine.Graph{
		Schema: 1,
		Nodes: []engine.GraphNode{
			{ID: "onoff", Type: engine.TypeSourceChannel, Params: map[string]any{"channel": "mqtt:" + stateTopic}},
			{ID: "cmd", Type: engine.TypeSinkChannel, Params: map[string]any{"channel": "mqtt:" + cmdTopic, "payload": "on-off"}},
			{ID: "level", Type: engine.TypeSourceChannelFloat, Params: map[string]any{"channel": "mqtt:src/level"}},
			{ID: "gain", Type: engine.TypeSinkChannelFloat, Params: map[string]any{"channel": "mqtt:" + setTopic, "payload": "json:gain"}},
		},
		Edges: []engine.GraphEdge{
			{From: "onoff:out", To: "cmd:in"},
			{From: "level:out", To: "gain:in"},
		},
	}
	eng, err := engine.Build(g, engine.DefaultRegistry(), 20*time.Millisecond)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	drv := NewDriver(cli, []engine.Channel{
		{Address: stateTopic, Kind: engine.Bool},
		{Address: cmdTopic, Kind: engine.Bool},
		{Address: "src/level", Kind: engine.Float},
		{Address: setTopic, Kind: engine.Float},
	}, nil)
	t.Cleanup(func() { drv.Close() })
	reg := engine.NewDriverRegistry()
	reg.RegisterSource(engine.PrefixMQTT, drv)
	reg.RegisterSink(engine.PrefixMQTT, drv)
	table := engine.BindingTable{
		"mqtt:" + stateTopic: {Prefix: engine.PrefixMQTT, Addr: stateTopic},
		"mqtt:" + cmdTopic:   {Prefix: engine.PrefixMQTT, Addr: cmdTopic},
		"mqtt:src/level":     {Prefix: engine.PrefixMQTT, Addr: "src/level"},
		"mqtt:" + setTopic:   {Prefix: engine.PrefixMQTT, Addr: setTopic},
	}
	configs := map[string]engine.ChannelConfig{
		"mqtt:" + cmdTopic: {"payload": "on-off"},
		"mqtt:" + setTopic: {"payload": "json:gain"},
	}
	if err := engine.BindGraph(eng, g, table, configs, reg); err != nil {
		t.Fatalf("BindGraph: %v", err)
	}

	cmd := make(chan []byte, 8)
	set := make(chan []byte, 8)
	subscribe := func(topic string, id int, ch chan []byte) {
		if err := cli.Subscribe(topic, id, func(_ string, p []byte) {
			b := make([]byte, len(p))
			copy(b, p)
			ch <- b
		}); err != nil {
			t.Fatalf("observe subscribe %s: %v", topic, err)
		}
		t.Cleanup(func() { _ = cli.Unsubscribe(topic, id) })
	}
	subscribe(cmdTopic, 9990, cmd)
	subscribe(setTopic, 9991, set)

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	await := func(ch chan []byte, want string) {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case raw := <-ch:
				if got := string(raw); got != want {
					t.Fatalf("payload = %q, want %q", got, want)
				}
				return
			case <-ticker.C:
				eng.Tick()
			case <-deadline:
				t.Fatalf("no %q within timeout", want)
			}
		}
	}
	// on/off control -> raw on/off on the command topic
	_ = cli.Publish(stateTopic, []byte("on"), false, 0)
	await(cmd, "on")
	_ = cli.Publish(stateTopic, []byte("off"), false, 0)
	await(cmd, "off")
	// a gain level -> {"gain":<n>} on the set topic
	_ = cli.Publish("src/level", []byte("50"), false, 0)
	await(set, `{"gain":50}`)
}

// nopInline satisfies the inline-client seam for bind-time-only tests.
type nopInline struct{}

func (nopInline) Publish(string, []byte, bool, byte) error          { return nil }
func (nopInline) Subscribe(string, int, func(string, []byte)) error { return nil }
func (nopInline) Unsubscribe(string, int) error                     { return nil }
