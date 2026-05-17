// Package weather wraps the open-meteo.com current-conditions API
// for the saison-14-01b mieter screensaver. The package is split
// across three files:
//
//   - wmo_codes.go: WMO weather-code -> Lucide-icon + German
//     description mapping (this file).
//   - cache.go:     15-minute in-memory cache with 24h stale-
//     serving so open-meteo outages do not slow
//     down browser reloads.
//   - openmeteo.go: thin REST client built on net/http.
package weather

// WMO weather codes per WMO Code 4677. Open-Meteo returns one of
// these as `current.weather_code`. We map the relevant subset to
// a Lucide icon name plus a short German label.
//
// The Lucide names are chosen from the icon set already bundled
// in carvilon-server's design-library; the same set ships with the
// ESP firmware so /esp/config can pass the icon name straight
// through.
//
// Codes not in this table fall back to "cloud" + "Wetter
// unbekannt". That keeps the screensaver visually consistent
// without us having to babysit every new code Open-Meteo invents.

type wmoEntry struct {
	icon        string
	description string
}

var wmoTable = map[int]wmoEntry{
	0:  {icon: "sun", description: "Klar"},
	1:  {icon: "cloud-sun", description: "Heiter"},
	2:  {icon: "cloud-sun", description: "Heiter"},
	3:  {icon: "cloud", description: "Bewoelkt"},
	45: {icon: "cloud-fog", description: "Nebel"},
	48: {icon: "cloud-fog", description: "Nebel mit Reif"},
	51: {icon: "cloud-drizzle", description: "Leichter Niesel"},
	53: {icon: "cloud-drizzle", description: "Niesel"},
	55: {icon: "cloud-drizzle", description: "Starker Niesel"},
	56: {icon: "cloud-drizzle", description: "Gefrierender Niesel"},
	57: {icon: "cloud-drizzle", description: "Starker gefrierender Niesel"},
	61: {icon: "cloud-rain", description: "Leichter Regen"},
	63: {icon: "cloud-rain", description: "Regen"},
	65: {icon: "cloud-rain", description: "Starker Regen"},
	66: {icon: "cloud-rain", description: "Gefrierender Regen"},
	67: {icon: "cloud-rain", description: "Starker gefrierender Regen"},
	71: {icon: "snowflake", description: "Leichter Schneefall"},
	73: {icon: "snowflake", description: "Schneefall"},
	75: {icon: "snowflake", description: "Starker Schneefall"},
	77: {icon: "snowflake", description: "Schneegriesel"},
	80: {icon: "cloud-rain-wind", description: "Leichte Schauer"},
	81: {icon: "cloud-rain-wind", description: "Schauer"},
	82: {icon: "cloud-rain-wind", description: "Heftige Schauer"},
	85: {icon: "snowflake", description: "Schneeschauer"},
	86: {icon: "snowflake", description: "Starke Schneeschauer"},
	95: {icon: "cloud-lightning", description: "Gewitter"},
	96: {icon: "cloud-lightning", description: "Gewitter mit Hagel"},
	99: {icon: "cloud-lightning", description: "Starkes Gewitter mit Hagel"},
}

// describeWMO returns the icon and description for code, falling
// back to a generic "cloud" / "Wetter unbekannt" pair for codes
// that are not in the WMO subset we curated.
func describeWMO(code int) (icon, description string) {
	if entry, ok := wmoTable[code]; ok {
		return entry.icon, entry.description
	}
	return "cloud", "Wetter unbekannt"
}
