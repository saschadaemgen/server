package shellycaps

import "testing"

func TestGen1Channels(t *testing.T) {
	cases := []struct {
		typeCode  string
		mode      string
		wantCount int
		wantMeter bool
	}{
		{"SHSW-1", "", 1, false}, // meters[] is the user constant, not a measurement
		{"SHSW-PM", "", 1, true}, // Shelly 1PM: one real meter
		{"SHSW-25", "relay", 2, true},
		{"SHSW-21", "relay", 2, false}, // one shared meter for two relays: unmetered until a shared surface exists
		{"SHPLG-S", "", 1, true},
		{"shsw-25", " Relay ", 2, true}, // case/whitespace tolerance on code AND mode
		{"  SHSW-PM  ", "", 1, true},
		{"SHXYZ-99", "", 1, false}, // unknown -> safe minimum, like Gen2
		{"", "", 1, false},
	}
	for _, c := range cases {
		got := Gen1Channels(c.typeCode, c.mode)
		if len(got) != c.wantCount {
			t.Errorf("Gen1Channels(%q, %q) = %d channels, want %d", c.typeCode, c.mode, len(got), c.wantCount)
			continue
		}
		for i, ch := range got {
			if ch.ID != i {
				t.Errorf("Gen1Channels(%q, %q)[%d].ID = %d, want %d", c.typeCode, c.mode, i, ch.ID, i)
			}
			if ch.Meter != c.wantMeter {
				t.Errorf("Gen1Channels(%q, %q)[%d].Meter = %v, want %v", c.typeCode, c.mode, i, ch.Meter, c.wantMeter)
			}
		}
	}
}

func TestGen1ChannelsRollerMode(t *testing.T) {
	// A Shelly 2/2.5 in roller mode drives a cover, not relays - offering
	// relay channels would actuate the wrong thing, so the set must be empty.
	for _, code := range []string{"SHSW-25", "SHSW-21", "shsw-25"} {
		for _, mode := range []string{"roller", "ROLLER", " roller "} {
			if got := Gen1Channels(code, mode); len(got) != 0 {
				t.Errorf("Gen1Channels(%q, %q) = %d channels, want none in roller mode", code, mode, len(got))
			}
		}
	}
	// Single-relay devices cannot be rollers; a stray mode string must not
	// erase their only channel.
	if got := Gen1Channels("SHSW-PM", "roller"); len(got) != 1 {
		t.Errorf("Gen1Channels(SHSW-PM, roller) = %d channels, want 1 (mode irrelevant for 1-relay)", len(got))
	}
}

func TestGen1ModelLabel(t *testing.T) {
	cases := []struct {
		typeCode string
		want     string
	}{
		{"SHSW-25", "Shelly 2.5"},
		{"shsw-25", "Shelly 2.5"}, // tolerant lookup
		{"  SHSW-1 ", "Shelly 1"},
		{"SHPLG-S", "Shelly Plug S"},
		{"SHXYZ-99", "SHXYZ-99"},   // unknown -> verbatim so the row still shows what the device said
		{" SHXYZ-99 ", "SHXYZ-99"}, // ... but trimmed
		{"", ""},
	}
	for _, c := range cases {
		if got := Gen1ModelLabel(c.typeCode); got != c.want {
			t.Errorf("Gen1ModelLabel(%q) = %q, want %q", c.typeCode, got, c.want)
		}
	}
}

func TestIsGen1Type(t *testing.T) {
	cases := []struct {
		typeCode string
		want     bool
	}{
		{"SHSW-1", true},
		{"SHSW-25", true},
		{"shplg-s", true}, // tolerant like the table lookups
		{" SHSW-PM ", true},
		{"SHDM-1", false},         // dimmer: deliberately out of the relay-class scope
		{"SPSW-104PE16EU", false}, // Gen2 code is not a Gen1 type
		{"", false},
	}
	for _, c := range cases {
		if got := IsGen1Type(c.typeCode); got != c.want {
			t.Errorf("IsGen1Type(%q) = %v, want %v", c.typeCode, got, c.want)
		}
	}
}

// TestGen1Lights pins the light-class capability: an RGBW2 in color mode
// drives one combined RGBW light, in white mode four independent white
// channels; relay-class codes have no lights at all.
func TestGen1Lights(t *testing.T) {
	color := Gen1Lights("SHRGBW2", "color")
	if len(color) != 1 || color[0].Kind != "color" || color[0].ID != 0 {
		t.Errorf("color mode = %+v, want one color light", color)
	}
	// an empty/odd mode renders the color default rather than nothing
	if got := Gen1Lights(" shrgbw2 ", ""); len(got) != 1 || got[0].Kind != "color" {
		t.Errorf("default mode = %+v, want the color shape", got)
	}
	white := Gen1Lights("SHRGBW2", "White")
	if len(white) != 4 {
		t.Fatalf("white mode = %+v, want four white channels", white)
	}
	for i, l := range white {
		if l.ID != i || l.Kind != "white" {
			t.Errorf("white[%d] = %+v", i, l)
		}
	}
	if got := Gen1Lights("SHSW-25", "relay"); got != nil {
		t.Errorf("relay-class device has lights: %+v", got)
	}
	// the RGBW2 exposes NO relay channels - its surface is the lights
	if got := Gen1Channels("SHRGBW2", "color"); len(got) != 0 {
		t.Errorf("SHRGBW2 relay channels = %+v, want none", got)
	}
	if Gen1ModelLabel("SHRGBW2") != "Shelly RGBW2" {
		t.Errorf("label = %q", Gen1ModelLabel("SHRGBW2"))
	}
}
