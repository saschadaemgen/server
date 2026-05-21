package viewermanager

import "strings"

// NormalizeName bringt einen Wohnungs-Namen auf die kanonische
// Vergleichs-Form fuer Login-Lookups und Uniqueness-Checks.
//
// Saison 13-02-FIX4-a-HOTFIX4: viewers haben keine separate
// username-Spalte mehr; der Name ist der Login. Mieter darf
// "Familie Mueller 2OG", "FAMILIE MUELLER 2OG" oder
// "familie  mueller  2og" eintippen - normalisiert ist alles
// dasselbe und matched denselben Eintrag.
//
//	expandGermanUmlauts(Dämgen) == Daemgen
//	ToLower(Familie Mueller 2OG) == familie mueller 2og
//	collapseWhitespace("Familie  Mueller") == "familie mueller"
//	TrimSpace("  Daemgen  ") == "daemgen"
//
// Wichtig: der DB-Eintrag `name` bleibt ORIGINAL (mit Umlaut,
// Grossschreibung, Mehrfach-Spaces). Nur der Vergleich
// normalisiert. So sieht der Admin in der Liste exakt was er
// eingegeben hat.
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
