package httpserver

import (
	"testing"

	"carvilon.local/server/internal/viewermanager"
)

// TestNameNormalizationDrift is a temporary characterization test
// for ARCH-2 (S16-01). It pins the current behavior of the two
// name normalizers that exist in parallel today:
//
//   - httpserver.normalizeForCompare (this package, private)
//   - viewermanager.NormalizeName    (exported, used by all
//     production callers)
//
// Both are supposed to canonicalize a Wohnungs-Name into its
// login-comparison form. If they ever drift, tenants get locked
// out or duplicate rows slip past the uniqueness check.
//
// This test exercises both functions against the same input list
// and fails on any divergence. The input set covers the cases the
// briefing called out (leading/trailing space, case, umlaut
// precomposed NFC, multi-inner-space, empty, whitespace-only) and
// a few defensive edges (NFD combining diaeresis, capital eszett
// U+1E9E, tab/newline as internal whitespace, punctuation).
//
// Once the unifying refactor lands, this file is folded into the
// new internal/normalize package's _test.go and the
// httpserver/username.go helpers go away with it.
func TestNameNormalizationDrift(t *testing.T) {
	inputs := []string{
		// Empty + whitespace-only
		"",
		" ",
		"   ",
		"\t",
		"\n",
		"\r",
		"\r\n",
		" \t \n ",

		// Leading/trailing whitespace
		" Wohnung 3 ",
		"  Wohnung 3  ",
		"\tWohnung 3\t",
		"\n Wohnung 3 \n",

		// Case folding
		"Wohnung 3",
		"wohnung 3",
		"WOHNUNG 3",
		"WoHnUnG 3",

		// Umlauts in precomposed NFC form
		"Mueller",
		"Müller",
		"MÜLLER",
		"Mueller-Hof",
		"Müller-Hof",
		"Dämgen",
		"DÄMGEN",
		"Möller",
		"Öl",
		"Über",
		"Straße 5",
		"STRASSE 5",
		"strasse 5",

		// Umlauts in NFD decomposed form (base char + combining
		// diaeresis U+0308). The Replacer only knows NFC codepoints;
		// this row exists to document that NFD passes through as-is.
		"Müller", // "Mu" + diaeresis = visually "Müller"
		"Dämgen", // "Da" + diaeresis = visually "Dämgen"

		// Capital eszett (U+1E9E) - not in either replacer table.
		"Straẞe",

		// Multiple internal spaces
		"Wohnung  3",
		"Wohnung   3",
		"Familie  Mueller   2OG",
		"a  b  c",
		"a    b    c",

		// Tab as internal whitespace
		"Familie\tMueller",
		"Familie \tMueller",
		"a\tb",
		"a\t\tb",

		// CR / LF as internal whitespace
		"a\nb",
		"a\rb",
		"a\r\nb",

		// Punctuation / digits / dashes
		"Mueller-Hof",
		"Mueller_Hof",
		"Mueller.Hof",
		"Wohnung #3",
		"2OG-links",
		"3.OG",
		"1.Stock links",

		// Combined edge cases
		"  FAMILIE  Müller   2OG  ",
		"\t Dämgen \n",
		"\r\n   Strasse   5   \r\n",
	}

	for _, in := range inputs {
		// t.Run gives us per-row failure isolation in the report.
		t.Run(in, func(t *testing.T) {
			gotHTTP := normalizeForCompare(in)
			gotVM := viewermanager.NormalizeName(in)
			if gotHTTP != gotVM {
				t.Errorf("drift on input %q:\n  normalizeForCompare       -> %q\n  viewermanager.NormalizeName -> %q",
					in, gotHTTP, gotVM)
			}
		})
	}
}
