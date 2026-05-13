package httpserver

import "testing"

func TestSanitizeUsername(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Daemgen", "daemgen"},
		{"daemgen", "daemgen"},
		{"DAEMGEN", "daemgen"},
		{"Dämgen", "daemgen"},
		{"DÄMGEN", "daemgen"},
		{"Möller Wohnung 2OG", "moeller-wohnung-2og"},
		{"Müller Müller", "mueller-mueller"},
		{"Straße 5", "strasse-5"},
		{"Hänsel.Gretel", "haensel.gretel"},
		{"   Daemgen   ", "daemgen"},
		{"  -Daemgen-  ", "daemgen"},
		{"a", "a-viewer"},                                    // unter 3 -> -viewer-Suffix
		{"verylongusernamethatkeepsgoingandgoingandgoing!!!!", "verylongusernamethatkeepsgoingan"}, // 32 chars max
		{"!!!", "viewer"},                                    // alles weggefiltert -> Suffix-only
		{"Familie Mueller 2OG", "familie-mueller-2og"},
		{"with  multiple   spaces", "with-multiple-spaces"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := sanitizeUsername(c.in)
			if got != c.want {
				t.Errorf("sanitizeUsername(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestExpandGermanUmlauts(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Daemgen", "Daemgen"},
		{"Dämgen", "Daemgen"},
		{"Möller", "Moeller"},
		{"Müller", "Mueller"},
		{"Straße", "Strasse"},
		{"ÄÖÜ", "aeoeue"},
		{"äöüß", "aeoeuess"},
	}
	for _, c := range cases {
		if got := expandGermanUmlauts(c.in); got != c.want {
			t.Errorf("expandGermanUmlauts(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
