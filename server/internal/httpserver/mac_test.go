package httpserver

import "testing"

func TestNormalizeMACToColonForm(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"28704e31e29c", "28:70:4e:31:e2:9c"},     // bare lowercase
		{"28704E31E29C", "28:70:4e:31:e2:9c"},     // bare uppercase
		{"28:70:4e:31:e2:9c", "28:70:4e:31:e2:9c"}, // colon already
		{"28:70:4E:31:E2:9C", "28:70:4e:31:e2:9c"}, // colon uppercase
		{"  28704e31e29c  ", "28:70:4e:31:e2:9c"},  // padded
		{"", ""},                                    // empty in, empty out
		{"not-a-mac", "not-a-mac"},                  // unknown shape passes through
		{"abcdef", "abcdef"},                        // wrong-length hex passes through
	}
	for _, c := range cases {
		got := normalizeMACToColonForm(c.in)
		if got != c.want {
			t.Errorf("normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMacAnyForm_Matches(t *testing.T) {
	matchCases := []string{
		"28704e31e29c",
		"28704E31E29C",
		"28:70:4e:31:e2:9c",
		"28:70:4E:31:E2:9C",
		"0c:ea:14:42:42:42",
	}
	for _, in := range matchCases {
		if !macAnyForm.MatchString(in) {
			t.Errorf("expected %q to match macAnyForm", in)
		}
	}
}

func TestMacAnyForm_Rejects(t *testing.T) {
	rejectCases := []string{
		"",                        // empty
		"abcdef",                  // too short
		"28704e31e29",             // 11 chars
		"28704e31e29c00",          // 14 chars
		"28-70-4e-31-e2-9c",       // dashes
		"door-uuid-1",             // door uuid
		"321e5134-b189-4de1-...",  // uuid prefix
	}
	for _, in := range rejectCases {
		if macAnyForm.MatchString(in) {
			t.Errorf("expected %q to NOT match macAnyForm", in)
		}
	}
}
