package httpserver

import "strings"

// normalizeForCompare brings a Wohnungs-Name down to its
// canonical comparison form. Both the create path and the login
// path go through this function; the stored `name` column keeps
// the ORIGINAL spelling (umlauts, case, spaces). Only the
// comparison value is normalised.
//
//	"Familie Mueller 2OG" == "familie mueller 2og"
//	"Dämgen"              == "Daemgen"               == "DAEMGEN"
//	"Müller  Hof"         == "mueller hof"           (collapsed whitespace)
//
// There is no separate username column; the Wohnungs-Name is
// used directly as the login.
func normalizeForCompare(s string) string {
	s = expandGermanUmlauts(s)
	s = strings.ToLower(s)
	s = strings.TrimSpace(s)
	s = collapseWhitespace(s)
	return s
}

// expandGermanUmlauts maps ä/ö/ü/ß and their uppercase variants
// to two-character ASCII equivalents.
func expandGermanUmlauts(s string) string {
	r := strings.NewReplacer(
		"ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss",
		"Ä", "ae", "Ö", "oe", "Ü", "ue",
	)
	return r.Replace(s)
}

// collapseWhitespace replaces every whitespace run (spaces,
// tabs, multiple spaces) with a single space so
// "Familie  Mueller" matches "Familie Mueller".
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
