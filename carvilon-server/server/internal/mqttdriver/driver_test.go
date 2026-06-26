package mqttdriver

import (
	"testing"

	"carvilon.local/server/internal/engine"
)

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
