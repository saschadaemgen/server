package shellycaps

import "testing"

func TestChannels(t *testing.T) {
	cases := []struct {
		model     string
		wantCount int
		wantMeter bool
	}{
		{"Shelly Pro4PM", 4, true},
		{"SPSW-104PE16EU", 4, true}, // raw Pro 4PM code
		{"Shelly Pro2PM", 2, true},
		{"Shelly Plus1PM", 1, true},
		{"Shelly Plus 1", 1, false}, // no PM: relay without metering
		{"Shelly Pro1", 1, false},
		{"", 1, false}, // unknown -> safe minimum
		{"weird-model", 1, false},
	}
	for _, c := range cases {
		got := Channels(c.model)
		if len(got) != c.wantCount {
			t.Errorf("Channels(%q) = %d channels, want %d", c.model, len(got), c.wantCount)
			continue
		}
		for i, ch := range got {
			if ch.ID != i {
				t.Errorf("Channels(%q)[%d].ID = %d, want %d", c.model, i, ch.ID, i)
			}
			if ch.Meter != c.wantMeter {
				t.Errorf("Channels(%q)[%d].Meter = %v, want %v", c.model, i, ch.Meter, c.wantMeter)
			}
		}
	}
}
