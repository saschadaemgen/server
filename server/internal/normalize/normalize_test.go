package normalize

import "testing"

// TestViewerName pins the canonical comparison form against the
// full input list the ARCH-2 characterization test ran against
// the two predecessor implementations. The expected outputs were
// produced by running viewermanager.NormalizeName (the live
// production normalizer at the time of extraction) against each
// row; the bit-identical drift test in commit 2dc883e confirms
// that the now-deleted httpserver.normalizeForCompare would have
// produced the same value for every row.
//
// If a future change to ViewerName ever causes one of these
// expected values to drift, the failure mode is:
//
//   - production tenants get locked out (login lookup misses the
//     existing row), or
//   - duplicate viewer rows slip past the uniqueness check.
//
// So: if a test in this file fails, do NOT update the want value
// without first explaining the new behavior in the briefing.
func TestViewerName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// Empty + whitespace-only
		{"", ""},
		{" ", ""},
		{"   ", ""},
		{"\t", ""},
		{"\n", ""},
		{"\r", ""},
		{"\r\n", ""},
		{" \t \n ", ""},

		// Leading/trailing whitespace
		{" Wohnung 3 ", "wohnung 3"},
		{"  Wohnung 3  ", "wohnung 3"},
		{"\tWohnung 3\t", "wohnung 3"},
		{"\n Wohnung 3 \n", "wohnung 3"},

		// Case folding
		{"Wohnung 3", "wohnung 3"},
		{"wohnung 3", "wohnung 3"},
		{"WOHNUNG 3", "wohnung 3"},
		{"WoHnUnG 3", "wohnung 3"},

		// Umlauts in precomposed NFC form
		{"Mueller", "mueller"},
		{"Müller", "mueller"},
		{"MÜLLER", "mueller"},
		{"Mueller-Hof", "mueller-hof"},
		{"Müller-Hof", "mueller-hof"},
		{"Dämgen", "daemgen"},
		{"DÄMGEN", "daemgen"},
		{"Möller", "moeller"},
		{"Öl", "oel"},
		{"Über", "ueber"},
		{"Straße 5", "strasse 5"},
		{"STRASSE 5", "strasse 5"},
		{"strasse 5", "strasse 5"},

		// Umlauts in NFD decomposed form (base char + combining
		// diaeresis U+0308). The replacer only knows precomposed
		// NFC codepoints; the combining sequence passes through.
		// Documenting this here so the behavior survives accidental
		// "let's normalize unicode first" rewrites.
		{"Müller", "müller"},
		{"Dämgen", "dämgen"},

		// Capital eszett (U+1E9E) - not in the replacer table, so
		// it survives ToLower to lowercase ß but is then no longer
		// expanded by this run (the expand step ran first). The
		// final value contains a lowercase ß.
		{"Straẞe", "straße"},

		// Multiple internal spaces
		{"Wohnung  3", "wohnung 3"},
		{"Wohnung   3", "wohnung 3"},
		{"Familie  Mueller   2OG", "familie mueller 2og"},
		{"a  b  c", "a b c"},
		{"a    b    c", "a b c"},

		// Tab as internal whitespace
		{"Familie\tMueller", "familie mueller"},
		{"Familie \tMueller", "familie mueller"},
		{"a\tb", "a b"},
		{"a\t\tb", "a b"},

		// CR / LF as internal whitespace
		{"a\nb", "a b"},
		{"a\rb", "a b"},
		{"a\r\nb", "a b"},

		// Punctuation / digits / dashes survive
		{"Mueller_Hof", "mueller_hof"},
		{"Mueller.Hof", "mueller.hof"},
		{"Wohnung #3", "wohnung #3"},
		{"2OG-links", "2og-links"},
		{"3.OG", "3.og"},
		{"1.Stock links", "1.stock links"},

		// Combined edge cases
		{"  FAMILIE  Müller   2OG  ", "familie mueller 2og"},
		{"\t Dämgen \n", "daemgen"},
		{"\r\n   Strasse   5   \r\n", "strasse 5"},
	}

	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := ViewerName(c.in); got != c.want {
				t.Errorf("ViewerName(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
