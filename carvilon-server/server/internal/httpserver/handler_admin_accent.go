// Saison 20 admin redesign: the single UI accent color.
//
// The accent is ONE value (hex "#rrggbb") in the existing platform_config
// KV store (key admin_accent_color) - no migration. resolveAccentColor feeds
// every admin render through navEnvelope.AccentColor; the nav/layout inject a
// :root{--accent;--color-accent} override so the whole admin reflects it. The
// later device setup wizard writes the same key.
package httpserver

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"carvilon.local/server/internal/platformconfig"
)

// DefaultAccentColor is the accent used when none has been chosen. Owner
// decision (S20): the default is blue; orange stays available as a preset.
const DefaultAccentColor = "#3d7bff"

var accentHexRE = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// accentPreset is one swatch in the /a/settings picker.
type accentPreset struct {
	Name string
	Hex  string
}

// accentPresets are the 10 one-click choices; the picker also offers a free
// color input for anything else.
var accentPresets = []accentPreset{
	{"Orange", "#ff7a1a"},
	{"Bernstein", "#f0a32a"},
	{"Blau", "#3d7bff"},
	{"Tuerkis", "#18b6c4"},
	{"Gruen", "#2cc66a"},
	{"Mint", "#34e29b"},
	{"Violett", "#8b5cf6"},
	{"Pink", "#ec4899"},
	{"Rot", "#ef4444"},
	{"Schiefer", "#64748b"},
}

// normalizeAccentHex validates a "#rrggbb" string and lowercases it.
func normalizeAccentHex(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if !accentHexRE.MatchString(s) {
		return "", false
	}
	return s, true
}

// resolveAccentColor returns the stored admin accent, or the orange default
// when unset/invalid/unwired. Always returns a valid hex so the override is
// applied uniformly (this is what flips the legacy pages to the chosen color).
func (s *Server) resolveAccentColor() string {
	if s.platformCfg == nil {
		return DefaultAccentColor
	}
	v, err := s.platformCfg.Get(context.Background(), platformconfig.KeyAdminAccentColor)
	if err != nil {
		s.log.Warn("resolve accent color", "err", err)
		return DefaultAccentColor
	}
	if hex, ok := normalizeAccentHex(v); ok {
		return hex
	}
	return DefaultAccentColor
}

// accentSettingsBlock is the picker payload for the settings page.
type accentSettingsBlock struct {
	Current string
	Presets []accentPreset
}

func (s *Server) accentSettingsBlock() accentSettingsBlock {
	return accentSettingsBlock{Current: s.resolveAccentColor(), Presets: accentPresets}
}

// handleAdminAccentPost stores the chosen accent (swatch button value or the
// free color input, both POST field "accent") and re-renders the settings page.
func (s *Server) handleAdminAccentPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if s.platformCfg == nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	hex, ok := normalizeAccentHex(r.PostForm.Get("accent"))
	data := s.buildSettingsData(r)
	if !ok {
		data.Flash = "Ungueltige Farbe. Erwartet wird ein Hex-Wert wie #ff7a1a."
		data.FlashType = "red"
		s.renderAdminPage(w, "settings", data)
		return
	}
	if err := s.platformCfg.Set(r.Context(), platformconfig.KeyAdminAccentColor, hex); err != nil {
		s.log.Error("save accent color", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data = s.buildSettingsData(r)
	data.Flash = "Akzentfarbe gespeichert."
	data.FlashType = "green"
	s.renderAdminPage(w, "settings", data)
}
