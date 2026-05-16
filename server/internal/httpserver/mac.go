package httpserver

import (
	"regexp"
	"strings"
)

// macAnyForm matches a MAC in either of the two spellings the
// /webviewer/doors/{intercom_mac}/unlock path could carry:
//
//	28:70:4e:31:e2:9c   colon-form (admin / mapping internal)
//	28704e31e29c        bare 12-hex (the live UA SSE payload)
//
// Case-insensitive. Used only by the mieter unlock path; the
// strict colon-form macFormat in handler_admin_web_viewers.go
// stays narrow because admin viewer operations always go through
// the colon spelling.
//
// Saison 13-05-HOTFIX5: live test 14 May 12:33 showed the
// browser POSTs the bare-12-hex form (the doorbell_start SSE
// frame's device_id), but the saved mapping is keyed in
// colon-form. Sniffing both lets the handler normalise before
// lookup.
var macAnyForm = regexp.MustCompile(
	`^(?:[0-9a-fA-F]{12}|(?:[0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2})$`,
)

// normalizeMACToColonForm rewrites a MAC in either bare 12-hex
// or already-colon form into canonical lowercase colon form
// (xx:xx:xx:xx:xx:xx). Inputs that are not MAC-shaped pass
// through unchanged so the caller can still match against a
// real door UUID, for example.
func normalizeMACToColonForm(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}
	if strings.Contains(s, ":") {
		return s
	}
	if len(s) != 12 {
		return s
	}
	var b strings.Builder
	b.Grow(17)
	for i := 0; i < 12; i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(s[i : i+2])
	}
	return b.String()
}
