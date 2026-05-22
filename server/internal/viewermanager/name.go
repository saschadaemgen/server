package viewermanager

import "strings"

// NormalizeName collapses a Wohnungs-Name down to its canonical
// comparison form, used for login lookups and uniqueness checks.
//
// Viewers no longer have a separate username column; the name is
// the login. A tenant typing "Familie Mueller 2OG", "FAMILIE
// MUELLER 2OG", or "familie  mueller  2og" must all hit the same
// row.
//
//	expandGermanUmlauts(Dämgen) == Daemgen
//	ToLower(Familie Mueller 2OG) == familie mueller 2og
//	collapseWhitespace("Familie  Mueller") == "familie mueller"
//	TrimSpace("  Daemgen  ") == "daemgen"
//
// The stored `name` column keeps the ORIGINAL spelling (umlauts,
// case, whitespace). Only the comparison value is normalised, so
// the admin sees in the list exactly what was typed in.
func NormalizeName(s string) string {
	s = expandGermanUmlautsForName(s)
	s = strings.ToLower(s)
	s = strings.TrimSpace(s)
	s = collapseWhitespaceForName(s)
	return s
}

func expandGermanUmlautsForName(s string) string {
	r := strings.NewReplacer(
		"ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss",
		"Ä", "ae", "Ö", "oe", "Ü", "ue",
	)
	return r.Replace(s)
}

func collapseWhitespaceForName(s string) string {
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
