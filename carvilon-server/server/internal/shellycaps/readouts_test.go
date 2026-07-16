package shellycaps

import "testing"

func tokens(rs []Readout) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Token)
	}
	return out
}

func TestReadouts_Gen2MeteredHasTheFullSet(t *testing.T) {
	got := Readouts("Shelly Plus1PM", false, "")
	want := []string{"sw0_power", "sw0_voltage", "sw0_current", "sw0_freq", "sw0_temp"}
	if len(got) != len(want) {
		t.Fatalf("Plus1PM readouts = %v, want %v", tokens(got), want)
	}
	for i, w := range want {
		if got[i].Token != w {
			t.Errorf("readout[%d] = %q, want %q", i, got[i].Token, w)
		}
	}
	// Units are what the charts label with; a wrong one silently mislabels a
	// real measurement.
	units := map[string]string{"sw0_power": "W", "sw0_voltage": "V", "sw0_current": "A", "sw0_freq": "Hz", "sw0_temp": "°C"}
	for _, r := range got {
		if r.Unit != units[r.Token] {
			t.Errorf("%s unit = %q, want %q", r.Token, r.Unit, units[r.Token])
		}
		if r.Label == "" {
			t.Errorf("%s has no label", r.Token)
		}
	}
}

func TestReadouts_Gen2MultiChannelPerChannel(t *testing.T) {
	got := Readouts("Shelly Pro4PM", false, "")
	if len(got) != 4*5 {
		t.Fatalf("Pro4PM = %d readouts, want 4 channels x 5", len(got))
	}
	if got[0].Token != "sw0_power" || got[15].Token != "sw3_power" {
		t.Errorf("per-channel tokens wrong: %v", tokens(got))
	}
	if got[15].Label != "CH4 power" {
		t.Errorf("CH4 label = %q", got[15].Label)
	}
}

// A plain switch reports no power, so it must declare nothing - a declared
// metric that never arrives would offer a chart that can never fill.
func TestReadouts_NonMeteredDeclaresNothing(t *testing.T) {
	if got := Readouts("Shelly Plus1", false, ""); len(got) != 0 {
		t.Errorf("non-metered Gen2 = %v, want none", tokens(got))
	}
	if got := Readouts("SHSW-1", true, ""); len(got) != 0 {
		t.Errorf("non-metered Gen1 = %v, want none", tokens(got))
	}
}

// Gen1 has no combined status payload: power arrives alone on its own topic
// and voltage/current/frequency do not exist there. Declaring them would
// promise data the device never sends.
func TestReadouts_Gen1MeteredIsPowerOnly(t *testing.T) {
	got := Readouts("SHSW-PM", true, "")
	if len(got) != 1 || got[0].Token != "sw0_power" || got[0].Unit != "W" {
		t.Fatalf("Gen1 1PM = %v, want sw0_power only", tokens(got))
	}
	// The 2.5 is metered and two-channel.
	if got := Readouts("SHSW-25", true, ""); len(got) != 2 ||
		got[0].Token != "sw0_power" || got[1].Token != "sw1_power" {
		t.Errorf("SHSW-25 = %v, want sw0_power + sw1_power", tokens(got))
	}
}

// With the mode known, each shape declares exactly its own lights. (The
// unknown-mode case, which is what the callers actually have, is
// TestReadouts_RGBW2UnknownModeCoversBothShapes.)
func TestReadouts_Gen1LightPower(t *testing.T) {
	got := Readouts("SHRGBW2", true, "color")
	if len(got) != 1 || got[0].Token != "li0_power" || got[0].Unit != "W" {
		t.Fatalf("RGBW2 color = %v, want li0_power", tokens(got))
	}
	// White mode splits into four independent lights.
	white := Readouts("SHRGBW2", true, "white")
	if len(white) != 4 || white[3].Token != "li3_power" {
		t.Fatalf("RGBW2 white = %v, want li0..li3_power", tokens(white))
	}
}

// A Plus Plug S meters, but its model string ("Shelly PlusPlugS") carries none
// of the PM/PE/EM markers the heuristic looked for, so it read as a plain
// switch and its power/voltage/current went undeclared - while the tap
// recorded them anyway, into a cockpit that offered no chart and no form.
func TestReadouts_MeteredPlugIsDeclared(t *testing.T) {
	for _, model := range []string{"Shelly PlusPlugS", "Shelly Plug S", "PlusPlugIT"} {
		got := Readouts(model, false, "")
		if len(got) == 0 {
			t.Errorf("%q declares nothing; a metered plug must declare its channel", model)
			continue
		}
		if got[0].Token != "sw0_power" || got[0].Unit != "W" {
			t.Errorf("%q first readout = %+v, want sw0_power in W", model, got[0])
		}
	}
	// A plain switch must stay undeclared - the fix must not make everything
	// look metered.
	if got := Readouts("Shelly Plus1", false, ""); len(got) != 0 {
		t.Errorf("plain switch = %v, want none", tokens(got))
	}
}

// The device set does not persist the Gen1 mode, so the caps are always asked
// with mode "". An RGBW2 in white mode publishes white/0..3/status and the tap
// records li0..li3_power: all four must carry a label and a unit, or three
// real recorded metrics chart as bare tokens.
func TestReadouts_RGBW2UnknownModeCoversBothShapes(t *testing.T) {
	got := Readouts("SHRGBW2", true, "")
	want := []string{"li0_power", "li1_power", "li2_power", "li3_power"}
	if len(got) != len(want) {
		t.Fatalf("RGBW2 unknown mode = %v, want %v", tokens(got), want)
	}
	for i, w := range want {
		if got[i].Token != w {
			t.Errorf("readout[%d] = %q, want %q", i, got[i].Token, w)
		}
		if got[i].Unit != "W" || got[i].Label == "" {
			t.Errorf("%s = %+v, want a labelled W", w, got[i])
		}
	}
	// A KNOWN mode still declares exactly that mode's shape.
	if c := Readouts("SHRGBW2", true, "color"); len(c) != 1 || c[0].Token != "li0_power" {
		t.Errorf("color mode = %v, want li0_power only", tokens(c))
	}
}
