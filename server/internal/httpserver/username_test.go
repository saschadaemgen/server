package httpserver

import "testing"

func TestNormalizeForCompare(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Familie Mueller 2OG", "familie mueller 2og"},
		{"FAMILIE MUELLER 2OG", "familie mueller 2og"},
		{"familie mueller 2og", "familie mueller 2og"},
		{"Familie  Mueller   2OG", "familie mueller 2og"},   // multi-WS
		{"  Familie Mueller 2OG  ", "familie mueller 2og"},  // padding
		{"Dämgen", "daemgen"},
		{"DÄMGEN", "daemgen"},
		{"Daemgen", "daemgen"},
		{"Möller", "moeller"},
		{"Müller-Hof", "mueller-hof"},
		{"Straße 5", "strasse 5"},
		{"Familie \tMueller", "familie mueller"}, // Tab als WS
		{"", ""},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := normalizeForCompare(c.in)
			if got != c.want {
				t.Errorf("normalizeForCompare(%q) = %q, want %q", c.in, got, c.want)
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

func TestCollapseWhitespace(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"a b", "a b"},
		{"a  b", "a b"},
		{"a   b", "a b"},
		{"a\tb", "a b"},
		{"a\t b", "a b"},
		{"a   b   c", "a b c"},
	}
	for _, c := range cases {
		if got := collapseWhitespace(c.in); got != c.want {
			t.Errorf("collapseWhitespace(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
