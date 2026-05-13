package httpserver

import "strings"

// sanitizeUsername bringt einen Anzeigenamen ODER eine Mieter-
// Login-Eingabe auf die kanonische Form, in der wir Usernames in
// der viewers-Tabelle ablegen:
//
//	"Familie Mueller 2OG"  -> "familie-mueller-2og"
//	"Familie Müller"       -> "familie-mueller"
//	"Daemgen"              -> "daemgen"
//	"Dämgen"               -> "daemgen"
//	"  Hänsel.Gretel  "    -> "haensel.gretel"
//
// Ablauf: TrimSpace -> Umlaute zu ae/oe/ue/ss -> lowercase ->
// nur a-z 0-9 _ . durchlassen, Whitespace/Sonderzeichen werden
// '-'. Doppelte '-' werden zu einem zusammengezogen, fuehrende/
// abschliessende '-' weggeschnitten. Anschliessend Mindest- und
// Maximal-Laenge (3..32).
//
// Saison 13-02-FIX4-a-HOTFIX3: diese Funktion ist die EINE
// kanonische Normalisierung. Sie laeuft sowohl beim Anlegen
// (Admin-Eingabe) als auch beim Login (Mieter-Eingabe), und die
// Migration 007 spiegelt einen Teil davon in SQL. Damit findet
// LookupByUsername mit exact-match das richtige Row, egal ob der
// Mieter "Dämgen", "Daemgen" oder "DAEMGEN" tippt.
func sanitizeUsername(name string) string {
	name = strings.TrimSpace(name)
	expanded := expandGermanUmlauts(name)
	var b strings.Builder
	prevDash := false
	for _, c := range strings.ToLower(expanded) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_':
			b.WriteRune(c)
			prevDash = false
		case c == ' ', c == '-', c == '\t':
			if !prevDash {
				b.WriteRune('-')
				prevDash = true
			}
		// alles andere (incl. Sonderzeichen, Umlaute-bereits-aufgeloest-
		// gibts hier nicht) wird verworfen.
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) < 3 {
		out = out + "-viewer"
		out = strings.Trim(out, "-")
	}
	if len(out) > 32 {
		out = out[:32]
	}
	return out
}

// expandGermanUmlauts ersetzt ä/ö/ü/ß und ihre Grossschreibungen
// durch die zwei-Zeichen-ASCII-Aequivalente. Wird sowohl direkt
// im sanitizeUsername aufgerufen als auch in der SQL-Migration
// (siehe migrations/007_normalize_viewer_usernames.sql) gespiegelt.
func expandGermanUmlauts(s string) string {
	r := strings.NewReplacer(
		"ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss",
		"Ä", "ae", "Ö", "oe", "Ü", "ue",
	)
	return r.Replace(s)
}
