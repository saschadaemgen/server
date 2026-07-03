package designer

import (
	"testing"

	"carvilon.local/server/internal/engine"
)

// TestCatalog_CountsAndCategories pins the palette shape the editor
// renders: 111 blocks across exactly the five categories, in the
// counts the former inline list had.
func TestCatalog_CountsAndCategories(t *testing.T) {
	blocks := Catalog(false, nil, false, false)
	if len(blocks) != 111 {
		t.Fatalf("Catalog(false, nil, false, false) has %d blocks, want 111", len(blocks))
	}
	want := map[string]int{"input": 26, "logic": 26, "time": 22, "memory": 13, "output": 24}
	got := map[string]int{}
	for _, b := range blocks {
		got[b.Category]++
	}
	if len(got) != len(want) {
		t.Errorf("categories = %v, want keys %v", got, want)
	}
	for cat, n := range want {
		if got[cat] != n {
			t.Errorf("category %q has %d blocks, want %d", cat, got[cat], n)
		}
	}
}

// TestCatalog_Shape asserts every block is well-formed: non-empty
// identity fields, unique types, never-nil port/param slices (so the
// JSON carries [] not null), and only valid kinds.
func TestCatalog_Shape(t *testing.T) {
	validKind := map[string]bool{"bool": true, "float": true, "text": true}
	seen := map[string]bool{}
	for _, b := range Catalog(true, nil, false, false) { // superset: also covers the GPIO blocks
		if b.Type == "" || b.Category == "" || b.Title == "" || b.Icon == "" {
			t.Errorf("block %+v has an empty identity field", b)
		}
		if seen[b.Type] {
			t.Errorf("duplicate type %q", b.Type)
		}
		seen[b.Type] = true
		if b.Inputs == nil || b.Outputs == nil || b.Params == nil {
			t.Errorf("block %q has a nil port/param slice (want empty)", b.Type)
		}
		for _, p := range append(append([]CatalogPort{}, b.Inputs...), b.Outputs...) {
			if p.Name == "" || !validKind[p.Kind] {
				t.Errorf("block %q port %+v invalid", b.Type, p)
			}
		}
		for _, p := range b.Params {
			if p.Name == "" || !validKind[p.Kind] {
				t.Errorf("block %q param %+v invalid", b.Type, p)
			}
		}
	}
}

// TestCatalog_Implemented checks that exactly the four engine-backed
// blocks are flagged implemented and that their ports/params/delay-
// boundary are derived faithfully from the engine registry (the single
// source of truth), while every other block stays catalog-only.
func TestCatalog_Implemented(t *testing.T) {
	wantImpl := map[string]bool{
		"input.manual":   true,
		"logic.or":       true,
		"time.staircase": true,
		"output.lamp":    true,
	}
	implCount := 0
	for _, b := range Catalog(false, nil, false, false) {
		if !b.Implemented {
			if len(b.Inputs) != 0 || len(b.Outputs) != 0 || len(b.Params) != 0 {
				t.Errorf("catalog-only block %q unexpectedly carries ports/params", b.Type)
			}
			continue
		}
		implCount++
		if !wantImpl[b.Type] {
			t.Errorf("unexpected implemented type %q", b.Type)
			continue
		}
		d, ok := engine.Lookup(b.Type)
		if !ok {
			t.Errorf("implemented type %q not in engine registry", b.Type)
			continue
		}
		if b.DelayBoundary != d.DelayBoundary {
			t.Errorf("%q delay_boundary = %v, want %v", b.Type, b.DelayBoundary, d.DelayBoundary)
		}
		assertPorts(t, b.Type, "inputs", b.Inputs, d.Inputs)
		assertPorts(t, b.Type, "outputs", b.Outputs, d.Outputs)
		if len(b.Params) != len(d.Params) {
			t.Errorf("%q has %d params, want %d", b.Type, len(b.Params), len(d.Params))
		} else {
			for i, p := range d.Params {
				if b.Params[i].Name != p.Name || b.Params[i].Kind != kindString(p.Kind) {
					t.Errorf("%q param %d = %+v, want name=%q kind=%q", b.Type, i, b.Params[i], p.Name, kindString(p.Kind))
				}
			}
		}
	}
	if implCount != len(wantImpl) {
		t.Errorf("implemented count = %d, want %d", implCount, len(wantImpl))
	}
}

// TestCatalog_GPIO verifies the GPIO category is gated on the runtime
// flag: absent without GPIO, and present (two engine-backed blocks typed
// to source.channel / sink.channel, with a user-set line param) with it.
func TestCatalog_GPIO(t *testing.T) {
	for _, b := range Catalog(false, nil, false, false) {
		if b.Category == "gpio" {
			t.Fatalf("gpio category present without GPIO: %+v", b)
		}
	}
	byType := map[string]CatalogBlock{}
	count := 0
	for _, b := range Catalog(true, nil, false, false) {
		if b.Category != "gpio" {
			continue
		}
		count++
		byType[b.Type] = b
		if !b.Implemented {
			t.Errorf("gpio block %q must be implemented (engine-backed)", b.Type)
		}
	}
	if count != 2 {
		t.Fatalf("gpio category has %d blocks, want 2", count)
	}
	src, ok := byType[engine.TypeSourceChannel]
	if !ok || len(src.Outputs) != 1 || src.Outputs[0].Kind != "bool" {
		t.Errorf("GPIO Eingang must be a %s with a bool output: %+v", engine.TypeSourceChannel, src)
	}
	if len(src.Params) != 1 || src.Params[0].Name != "channel" {
		t.Errorf("GPIO Eingang must expose the channel (line) param: %+v", src.Params)
	}
	snk, ok := byType[engine.TypeSinkChannel]
	if !ok || len(snk.Inputs) != 1 || snk.Inputs[0].Kind != "bool" {
		t.Errorf("GPIO Ausgang must be a %s with a bool input: %+v", engine.TypeSinkChannel, snk)
	}
	if a, b := len(Catalog(false, nil, false, false)), len(Catalog(true, nil, false, false)); b != a+2 {
		t.Errorf("Catalog(true) = %d blocks, want Catalog(false)+2 = %d", b, a+2)
	}
}

// TestCatalog_MQTT verifies the MQTT category is gated on the broker
// running: absent when off, two generic blocks (one source, one sink)
// when on.
func TestCatalog_MQTT(t *testing.T) {
	for _, b := range Catalog(false, nil, false, false) {
		if b.Category == "mqtt" {
			t.Fatalf("mqtt category present while broker off: %+v", b)
		}
	}
	var mqtt []CatalogBlock
	for _, b := range Catalog(false, nil, true, false) {
		if b.Category == "mqtt" {
			mqtt = append(mqtt, b)
		}
	}
	if len(mqtt) != 2 {
		t.Fatalf("mqtt category has %d blocks, want 2", len(mqtt))
	}
	var haveSrc, haveSink bool
	for _, b := range mqtt {
		switch b.Type {
		case engine.TypeSourceChannelFloat:
			haveSrc = true
		case engine.TypeSinkChannelFloat:
			haveSink = true
		}
	}
	if !haveSrc || !haveSink {
		t.Errorf("mqtt category must offer a source and a sink block: %+v", mqtt)
	}
}

// TestCatalog_Telegram verifies the Telegram category is gated on the
// bot running (enabled + token set): absent when off, four blocks -
// the bool send sink and command source for the two demo flows, plus
// the raw text source and text sink - when on, all engine-backed via
// the generic channel node types.
func TestCatalog_Telegram(t *testing.T) {
	for _, b := range Catalog(false, nil, false, false) {
		if b.Category == "telegram" {
			t.Fatalf("telegram category present while bot off: %+v", b)
		}
	}
	byTitle := map[string]CatalogBlock{}
	for _, b := range Catalog(false, nil, false, true) {
		if b.Category != "telegram" {
			continue
		}
		byTitle[b.Title] = b
		if !b.Implemented {
			t.Errorf("telegram block %q must be implemented (engine-backed)", b.Title)
		}
	}
	if len(byTitle) != 4 {
		t.Fatalf("telegram category has %d blocks, want 4: %v", len(byTitle), byTitle)
	}
	wantTypes := map[string]string{
		"Telegram Senden":      engine.TypeSinkChannel,
		"Telegram Befehl":      engine.TypeSourceChannel,
		"Telegram Empfangen":   engine.TypeSourceChannelText,
		"Telegram Text senden": engine.TypeSinkChannelText,
	}
	for title, typ := range wantTypes {
		b, ok := byTitle[title]
		if !ok {
			t.Errorf("telegram block %q missing", title)
			continue
		}
		if b.Type != typ {
			t.Errorf("telegram block %q type = %q, want %q", title, b.Type, typ)
		}
	}
}

// sampleSysMetrics is a fixed metric set for the system-category test.
func sampleSysMetrics() []SysMetric {
	return []SysMetric{
		{Address: "sys:cpu_temp", Label: "CPU-Temperatur", Unit: "°C"},
		{Address: "sys:ram", Label: "RAM-Auslastung", Unit: "%"},
	}
}

// TestCatalog_System verifies the system category is data-driven and gated
// on available metrics: absent with none, and one source.channel.float
// block per metric (with its physical ref baked into Channel and a unit)
// when present.
func TestCatalog_System(t *testing.T) {
	for _, b := range Catalog(true, nil, false, false) {
		if b.Category == "system" {
			t.Fatalf("system category present without metrics: %+v", b)
		}
	}
	metrics := sampleSysMetrics()
	var sys []CatalogBlock
	for _, b := range Catalog(false, metrics, false, false) {
		if b.Category == "system" {
			sys = append(sys, b)
		}
	}
	if len(sys) != len(metrics) {
		t.Fatalf("system category has %d blocks, want %d (one per metric)", len(sys), len(metrics))
	}
	for i, b := range sys {
		if b.Type != engine.TypeSourceChannelFloat {
			t.Errorf("system block %q type = %q, want %s", b.Title, b.Type, engine.TypeSourceChannelFloat)
		}
		if !b.Implemented {
			t.Errorf("system block %q must be implemented (engine-backed)", b.Title)
		}
		if b.Channel != metrics[i].Address || b.Unit != metrics[i].Unit {
			t.Errorf("system block %q = {channel:%q unit:%q}, want {%q %q}", b.Title, b.Channel, b.Unit, metrics[i].Address, metrics[i].Unit)
		}
		if len(b.Outputs) != 1 || b.Outputs[0].Kind != "float" {
			t.Errorf("system block %q must have one float output: %+v", b.Title, b.Outputs)
		}
	}
}

func assertPorts(t *testing.T, typ, side string, got []CatalogPort, want []engine.Port) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%q %s has %d ports, want %d", typ, side, len(got), len(want))
		return
	}
	for i, p := range want {
		if got[i].Name != p.Name || got[i].Kind != kindString(p.Kind) {
			t.Errorf("%q %s[%d] = %+v, want name=%q kind=%q", typ, side, i, got[i], p.Name, kindString(p.Kind))
		}
	}
}
