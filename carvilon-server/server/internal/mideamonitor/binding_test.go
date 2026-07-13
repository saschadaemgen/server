package mideamonitor

import (
	"context"
	"testing"
	"time"

	"carvilon.local/server/internal/engine"
	"carvilon.local/server/internal/mideastore"
)

func TestParseChan(t *testing.T) {
	// parseChan sees the PREFIX-STRIPPED "<id>:<token>" (the engine strips
	// "midea:" via ParsePhysical before calling the driver).
	cases := []struct {
		in, id, tok string
		ok          bool
	}{
		{"abc:setpoint", "abc", "setpoint", true},
		{"1f2e:device_temp", "1f2e", "device_temp", true},
		{"abc", "", "", false}, // no token
		{":x", "", "", false},  // empty id rejected (i<=0 guard)
	}
	for _, c := range cases {
		id, tok, ok := parseChan(c.in)
		if ok != c.ok {
			t.Errorf("parseChan(%q) ok=%v, want %v", c.in, ok, c.ok)
			continue
		}
		if ok && (id != c.id || tok != c.tok) {
			t.Errorf("parseChan(%q) = (%q,%q), want (%q,%q)", c.in, id, tok, c.id, c.tok)
		}
	}
}

// TestBindingCapabilities: an adopted device exposes 2 readout (source) + 3
// control (sink) channels. The CATALOG channel refs are full ("midea:<id>:..")
// but the engine-facing channel addresses are prefix-STRIPPED ("<id>:.."), and
// Subscribe/Write are called with the stripped form.
func TestBindingCapabilities(t *testing.T) {
	fs := &fakeStore{active: []mideastore.Device{
		{ID: "abc", DeviceID: 0xABC, Address: "192.0.2.5", State: mideastore.StateActive},
	}}
	m := New(fs, nil, WithInterval(time.Hour))
	m.tick(context.Background()) // register the device in m.devs

	devs := m.Devices()
	if len(devs) != 1 {
		t.Fatalf("Devices = %d, want 1", len(devs))
	}
	d := devs[0]
	if len(d.Readouts) != 2 || len(d.Controls) != 3 {
		t.Fatalf("caps: %d readouts, %d controls (want 2/3)", len(d.Readouts), len(d.Controls))
	}
	// Catalog-facing refs stay FULL (baked into the editor node param).
	if d.Controls[0].Channel != "midea:abc:setpoint" {
		t.Errorf("catalog setpoint channel = %q, want midea:abc:setpoint", d.Controls[0].Channel)
	}
	if d.Readouts[0].Channel != "midea:abc:device_temp" {
		t.Errorf("catalog device_temp channel = %q, want midea:abc:device_temp", d.Readouts[0].Channel)
	}

	b := m.NewRunBinding()
	defer b.Close()
	// Engine-facing channel addresses are PREFIX-STRIPPED (this is what
	// ParsePhysical hands the driver; the earlier full-ref form broke every run).
	chans := b.Channels()
	if len(chans) != 5 { // 2 readouts + 3 controls (union)
		t.Errorf("Channels = %d, want 5", len(chans))
	}
	haveStripped := false
	for _, c := range chans {
		if c.Address == "abc:setpoint" {
			haveStripped = true
		}
		if c.Address == "midea:abc:setpoint" {
			t.Errorf("engine channel address is full-prefixed %q; must be stripped", c.Address)
		}
	}
	if !haveStripped {
		t.Error("engine channels missing the stripped address abc:setpoint")
	}

	// Subscribe: readout ok, control rejected. (Stripped addrs, as the engine passes.)
	if err := b.Subscribe("abc:device_temp", func(engine.Value) {}); err != nil {
		t.Errorf("subscribe readout: %v", err)
	}
	if err := b.Subscribe("abc:setpoint", func(engine.Value) {}); err == nil {
		t.Error("subscribe on a control channel should error")
	}
	// Write: controls ok (non-blocking enqueue), readout + unknown rejected.
	if err := b.Write("abc:setpoint", engine.FloatVal(22)); err != nil {
		t.Errorf("write setpoint: %v", err)
	}
	if err := b.Write("abc:mode", engine.TextVal("cool")); err != nil {
		t.Errorf("write mode: %v", err)
	}
	if err := b.Write("abc:fan_mode", engine.TextVal("high")); err != nil {
		t.Errorf("write fan_mode: %v", err)
	}
	if err := b.Write("abc:device_temp", engine.FloatVal(1)); err == nil {
		t.Error("write on a readout channel should error")
	}
	if err := b.Write("abc:bogus", engine.FloatVal(1)); err == nil {
		t.Error("write on an unknown token should error")
	}
}
