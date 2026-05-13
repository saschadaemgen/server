package httpserver

import (
	"html/template"
)

// phosphorAlias maps the historical Lucide-flavoured icon names
// used across our templates to the equivalent Phosphor icon name.
// We picked Phosphor (regular weight) for Saison 13-02-FIX1b
// because Lucide felt sketchy next to the Apple-grade chrome the
// rest of the UI now wears; Phosphor's regular weight has the
// Apple SF Symbols silhouette without paying for SF Symbols
// licensing.
//
// The font itself is loaded once via the CDN link wired in
// templates/theme_head.html. Each icon renders as
// <i class="ph ph-NAME" aria-hidden="true"></i>; size is driven
// by the surrounding font-size in theme.css (.ph selectors).
var phosphorAlias = map[string]string{
	// Bell family.
	"bell":       "bell",
	"bell-ring":  "bell-ringing",
	"bell-slash": "bell-slash",

	// Call / door actions.
	"phone":     "phone",
	"phone-off": "phone-x",
	"key":       "key",

	// Header / nav.
	"home":     "house",
	"server":   "hard-drives",
	"settings": "gear-six",
	"users":    "users-three",
	"log-out":  "sign-out",
	"sparkle":  "sparkle",

	// Theme.
	"sun":  "sun",
	"moon": "moon",

	// Table / action verbs.
	"plus":          "plus",
	"trash":         "trash",
	"check":         "check",
	"x":             "x",
	"link":          "link",
	"copy":          "copy",
	"clipboard":     "clipboard",
	"clock":         "clock",
	"chevron-right": "caret-right",
}

// renderIcon returns the inline markup for the named icon. The
// look-up is intentionally forgiving: an unknown name returns
// empty so a template typo does not crash the render path. Names
// already in Phosphor form pass through unchanged.
func renderIcon(name string) template.HTML {
	target := name
	if alias, ok := phosphorAlias[name]; ok {
		target = alias
	}
	// aria-hidden because we ship a label next to the icon
	// wherever a screen reader needs context; decorative usage
	// (e.g. inside a button with aria-label) wants the icon out
	// of the accessibility tree.
	return template.HTML(`<i class="ph ph-` + target + `" aria-hidden="true"></i>`)
}
