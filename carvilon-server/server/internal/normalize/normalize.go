// Package normalize is the single source of truth for the
// canonical comparison form of viewer-/Wohnungs-names.
//
// The stored `name` column in viewers keeps the ORIGINAL spelling
// (umlauts, case, whitespace) so the admin sees what was typed.
// Only the comparison value used for login lookups and uniqueness
// checks is normalised through ViewerName here.
//
// History: this package replaces a pair of byte-identical
// implementations that drifted naturally apart (one in
// internal/httpserver/username.go, one in
// internal/viewermanager/name.go). Both had the same algorithm
// but lived in two files - a future hand could have changed one
// without the other and silently locked tenants out of their
// login. ARCH-2 (S16-01) collapsed both into this package; the
// pre-extract characterization test sits in git history as
// 2dc883e for any future drift investigation.
package normalize

import "strings"

// ViewerName brings a Wohnungs-Name down to its canonical
// comparison form.
//
//	"Familie Mueller 2OG"  == "familie mueller 2og"
//	"Dämgen"               == "Daemgen"               == "DAEMGEN"
//	"Müller  Hof"          == "mueller hof"   (collapsed whitespace)
//
// Steps, in order:
//  1. Expand the German umlauts (ä→ae, ö→oe, ü→ue, ß→ss) and
//     their uppercase precomposed-NFC variants. Bytes outside
//     that fixed table (NFD combining sequences, capital eszett
//     U+1E9E, anything else) pass through untouched.
//  2. ToLower.
//  3. TrimSpace (leading/trailing only).
//  4. Collapse every run of ASCII whitespace (space, tab, LF, CR)
//     down to a single space.
//
// There is no separate username column; the viewer name IS the
// login.
func ViewerName(s string) string {
	s = expandGermanUmlauts(s)
	s = strings.ToLower(s)
	s = strings.TrimSpace(s)
	s = collapseWhitespace(s)
	return s
}

// expandGermanUmlauts maps the precomposed NFC umlaut codepoints
// to their two-character ASCII equivalents. Uppercase variants
// fold to lowercase to keep the result aligned with the ToLower
// step that follows.
func expandGermanUmlauts(s string) string {
	r := strings.NewReplacer(
		"ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss",
		"Ä", "ae", "Ö", "oe", "Ü", "ue",
	)
	return r.Replace(s)
}

// collapseWhitespace replaces every run of ASCII whitespace
// (space, tab, LF, CR) with a single space so "Familie  Mueller"
// matches "Familie Mueller". Non-ASCII whitespace (NBSP and the
// rest of unicode.IsSpace) is intentionally NOT collapsed - the
// historical implementations both restricted themselves to the
// ASCII four, and changing that would be a behavior shift, not
// a refactor.
func collapseWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inWS := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !inWS {
				b.WriteByte(' ')
				inWS = true
			}
			continue
		}
		b.WriteRune(r)
		inWS = false
	}
	return b.String()
}
