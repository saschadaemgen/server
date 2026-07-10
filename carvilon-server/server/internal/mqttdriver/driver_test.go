package mqttdriver

import (
	"encoding/json"
	"testing"

	"carvilon.local/server/internal/engine"
)

func TestSplitAddr(t *testing.T) {
	cases := []struct{ addr, topic, sel string }{
		{"carvilon/x/online", "carvilon/x/online", ""},
		{"carvilon/shelly-ab/status/switch:0#output", "carvilon/shelly-ab/status/switch:0", "output"},
		{"carvilon/shelly-ab/status/switch:3#temperature.tC", "carvilon/shelly-ab/status/switch:3", "temperature.tC"},
		{"carvilon/shelly-ab/rpc#Switch.Set:2", "carvilon/shelly-ab/rpc", "Switch.Set:2"},
		{"a#", "a", ""},
	}
	for _, c := range cases {
		topic, sel := splitAddr(c.addr)
		if topic != c.topic || sel != c.sel {
			t.Errorf("splitAddr(%q) = (%q,%q), want (%q,%q)", c.addr, topic, sel, c.topic, c.sel)
		}
	}
}

// TestExtractPath walks a Shelly switch:0 status snapshot, including the
// nested temperature/aenergy fields the briefing calls out.
func TestExtractPath(t *testing.T) {
	payload := []byte(`{"id":0,"output":false,"apower":0.0,"voltage":225.9,` +
		`"aenergy":{"total":10366308.79},"temperature":{"tC":51.1,"tF":124.1}}`)
	cases := []struct {
		path  []string
		want  any
		found bool
	}{
		{[]string{"output"}, false, true},
		{[]string{"apower"}, 0.0, true},
		{[]string{"voltage"}, 225.9, true},
		{[]string{"temperature", "tC"}, 51.1, true},
		{[]string{"aenergy", "total"}, 10366308.79, true},
		{[]string{"missing"}, nil, false},
		{[]string{"temperature", "nope"}, nil, false},
		{[]string{"output", "tooDeep"}, nil, false}, // scalar, cannot descend
	}
	for _, c := range cases {
		got, found := extractPath(payload, c.path)
		if found != c.found {
			t.Errorf("extractPath(%v) found=%v, want %v", c.path, found, c.found)
			continue
		}
		if found && got != c.want {
			t.Errorf("extractPath(%v) = %v, want %v", c.path, got, c.want)
		}
	}
	if _, found := extractPath([]byte("not json"), []string{"x"}); found {
		t.Error("extractPath on non-JSON should not find")
	}
}

func TestCastValue(t *testing.T) {
	cases := []struct {
		kind    engine.Kind
		leaf    any
		wantOK  bool
		wantVal engine.Value
	}{
		{engine.Bool, false, true, engine.BoolVal(false)},
		{engine.Bool, true, true, engine.BoolVal(true)},
		{engine.Bool, float64(1), true, engine.BoolVal(true)},
		{engine.Bool, float64(0), true, engine.BoolVal(false)},
		{engine.Bool, "on", true, engine.BoolVal(true)},
		{engine.Bool, map[string]any{}, false, engine.Value{}},
		{engine.Float, 51.1, true, engine.FloatVal(51.1)},
		{engine.Float, "23.5", true, engine.FloatVal(23.5)},
		{engine.Float, true, true, engine.FloatVal(1)},
		{engine.Float, "abc", false, engine.Value{}},
		{engine.Text, "hi", true, engine.TextVal("hi")},
		{engine.Text, 12.5, true, engine.TextVal("12.5")},
		{engine.Text, true, true, engine.TextVal("true")},
		{engine.Text, map[string]any{}, false, engine.Value{}},
	}
	for _, c := range cases {
		v, ok := castValue(c.kind, c.leaf)
		if ok != c.wantOK {
			t.Errorf("castValue(%s,%v) ok=%v, want %v", kindName(c.kind), c.leaf, ok, c.wantOK)
			continue
		}
		if ok && v != c.wantVal {
			t.Errorf("castValue(%s,%v) = %+v, want %+v", kindName(c.kind), c.leaf, v, c.wantVal)
		}
	}
}

func TestParseRPCSelector(t *testing.T) {
	ok := []struct {
		sel  string
		spec rpcSpec
	}{
		{"Switch.Set:0", rpcSpec{"Switch.Set", 0}},
		{"Switch.Set:3", rpcSpec{"Switch.Set", 3}},
	}
	for _, c := range ok {
		got, err := parseRPCSelector(c.sel)
		if err != nil || got != c.spec {
			t.Errorf("parseRPCSelector(%q) = %+v, %v; want %+v", c.sel, got, err, c.spec)
		}
	}
	bad := []string{"", "Switch.Set", "Switch.Set:", ":0", "Switch.Set:x", "Switch.Set:-1", "Cover.Open:0"}
	for _, s := range bad {
		if _, err := parseRPCSelector(s); err == nil {
			t.Errorf("parseRPCSelector(%q) should error", s)
		}
	}
}

// TestFormatRPC checks the on/off envelope shape the Shelly Gen2 RPC
// expects, decoding it back so key order is irrelevant.
func TestFormatRPC(t *testing.T) {
	for _, on := range []bool{true, false} {
		raw := formatRPC(rpcSpec{"Switch.Set", 2}, "carvilon/shelly-ab/rpc", 7, on)
		var got struct {
			ID     int64  `json:"id"`
			Src    string `json:"src"`
			Method string `json:"method"`
			Params struct {
				ID int  `json:"id"`
				On bool `json:"on"`
			} `json:"params"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("formatRPC produced invalid JSON: %v (%s)", err, raw)
		}
		if got.ID != 7 || got.Method != "Switch.Set" || got.Src != "carvilon/shelly-ab/rpc/resp" {
			t.Errorf("envelope = %+v", got)
		}
		if got.Params.ID != 2 || got.Params.On != on {
			t.Errorf("params = %+v, want id=2 on=%v", got.Params, on)
		}
	}
}

func TestParsePayload(t *testing.T) {
	cases := []struct {
		kind    engine.Kind
		in      string
		wantOK  bool
		wantVal engine.Value
	}{
		{engine.Float, "23.5", true, engine.FloatVal(23.5)},
		{engine.Float, "  -1.0 ", true, engine.FloatVal(-1)},
		{engine.Float, "0", true, engine.FloatVal(0)},
		{engine.Float, "abc", false, engine.Value{}},
		{engine.Bool, "true", true, engine.BoolVal(true)},
		{engine.Bool, "ON", true, engine.BoolVal(true)},
		{engine.Bool, "1", true, engine.BoolVal(true)},
		{engine.Bool, "false", true, engine.BoolVal(false)},
		{engine.Bool, "off", true, engine.BoolVal(false)},
		{engine.Bool, "0", true, engine.BoolVal(false)},
		{engine.Bool, "maybe", false, engine.Value{}},
		{engine.Text, "hello world", true, engine.TextVal("hello world")},
		{engine.Text, "", true, engine.TextVal("")},
	}
	for _, c := range cases {
		v, ok := parsePayload(c.kind, []byte(c.in))
		if ok != c.wantOK {
			t.Errorf("parsePayload(%s,%q) ok=%v, want %v", kindName(c.kind), c.in, ok, c.wantOK)
			continue
		}
		if ok && v != c.wantVal {
			t.Errorf("parsePayload(%s,%q) = %+v, want %+v", kindName(c.kind), c.in, v, c.wantVal)
		}
	}
}

func TestFormatPayload(t *testing.T) {
	cases := []struct {
		v    engine.Value
		want string
	}{
		{engine.BoolVal(true), "true"},
		{engine.BoolVal(false), "false"},
		{engine.FloatVal(23.5), "23.5"},
		{engine.FloatVal(0), "0"},
		{engine.FloatVal(-1), "-1"},
		{engine.TextVal("hi"), "hi"},
	}
	for _, c := range cases {
		if got := string(formatPayload(c.v)); got != c.want {
			t.Errorf("formatPayload(%+v) = %q, want %q", c.v, got, c.want)
		}
	}
}

// TestRoundTrip: format then parse returns the original value, so an
// engine Out feeding an engine In over a topic is lossless.
func TestRoundTrip(t *testing.T) {
	for _, v := range []engine.Value{
		engine.BoolVal(true), engine.BoolVal(false),
		engine.FloatVal(3.14159), engine.FloatVal(-42),
		engine.TextVal("carvilon"),
	} {
		got, ok := parsePayload(v.Kind, formatPayload(v))
		if !ok || got != v {
			t.Errorf("round-trip %+v -> %+v (ok=%v)", v, got, ok)
		}
	}
}
