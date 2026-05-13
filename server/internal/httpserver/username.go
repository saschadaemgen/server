package httpserver

import "strings"

// normalizeForCompare bringt einen Wohnungs-Namen auf eine
// kanonische Vergleichs-Form. Beide Seiten (Anlegen und Login)
// gehen durch diese Funktion; der gespeicherte name bleibt
// ORIGINAL (mit Umlaut, Grossschreibung, Leerzeichen). Nur der
// Vergleich normalisiert.
//
//	"Familie Mueller 2OG"  == "familie mueller 2og"
//	"Dämgen"               == "Daemgen"               == "DAEMGEN"
//	"Müller  Hof"          == "mueller hof"           (doppel-Whitespace zusammen)
//
// Saison 13-02-FIX4-a-HOTFIX4 hat die separate Username-Spalte
// abgeschafft und nutzt den Wohnungs-Namen direkt als Login.
func normalizeForCompare(s string) string {
	s = expandGermanUmlauts(s)
	s = strings.ToLower(s)
	s = strings.TrimSpace(s)
	s = collapseWhitespace(s)
	return s
}

// expandGermanUmlauts ersetzt ä/ö/ü/ß und ihre Grossschreibungen
// durch die zwei-Zeichen-ASCII-Aequivalente.
func expandGermanUmlauts(s string) string {
	r := strings.NewReplacer(
		"ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss",
		"Ä", "ae", "Ö", "oe", "Ü", "ue",
	)
	return r.Replace(s)
}

// collapseWhitespace ersetzt jede Whitespace-Sequenz (Spaces,
// Tabs, Mehrfach-Spaces) durch einen einzelnen Space. So
// matched "Familie  Mueller" gegen "Familie Mueller".
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
