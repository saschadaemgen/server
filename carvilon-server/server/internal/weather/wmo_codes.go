// Package weather wraps the open-meteo.com current-conditions API
// for the saison-14-01b mieter screensaver. The package is split
// across three files:
//
//   - wmo_codes.go: WMO weather-code -> Lucide-icon + bilingual
//     description mapping (this file).
//   - cache.go:     15-minute in-memory cache with 24h stale-
//     serving so open-meteo outages do not slow
//     down browser reloads.
//   - openmeteo.go: thin REST client built on net/http.
package weather

// WMO weather codes per WMO Code 4677. Open-Meteo returns one of
// these as `current.weather_code`. We map the relevant subset to
// a Lucide icon name plus a short bilingual description.
//
// The Lucide names are chosen from the icon set already bundled
// in carvilon-server's design-library; the same set ships with the
// ESP firmware so /esp/config can pass the icon name straight
// through.
//
// Codes not in this table fall back to "cloud" + "Unbekannt" /
// "Unknown". That keeps the screensaver visually consistent
// without us having to babysit every new code Open-Meteo invents.
//
// Saison 14-FIX07: the table previously kept a single German
// description per code, hardcoded as ASCII without umlauts. The
// rebuilt table carries the icon, a proper-UTF-8 German label,
// and an English label so resolveWeather can localize per viewer
// language. icon names are unchanged so the ESP firmware's
// existing icon mapping keeps working.

// WeatherDesc bundles the per-WMO-code icon plus its German and
// English label. The struct is exported so other packages can
// inspect the curated set if they ever need to (current callers
// only go through resolveWeather).
type WeatherDesc struct {
	Icon string
	DE   string
	EN   string
}

// wmoTable is the curated WMO-code -> (icon + DE + EN) map. The
// list mirrors Open-Meteo's documented weather_code values; codes
// outside this set fall through to the resolveWeather fallback.
var wmoTable = map[int]WeatherDesc{
	0:  {Icon: "sun", DE: "Klar", EN: "Clear sky"},
	1:  {Icon: "sun", DE: "Überwiegend klar", EN: "Mainly clear"},
	2:  {Icon: "cloud-sun", DE: "Teils bewölkt", EN: "Partly cloudy"},
	3:  {Icon: "cloud", DE: "Bewölkt", EN: "Overcast"},
	45: {Icon: "cloud-fog", DE: "Nebel", EN: "Fog"},
	48: {Icon: "cloud-fog", DE: "Reifnebel", EN: "Depositing rime fog"},
	51: {Icon: "cloud-drizzle", DE: "Leichter Nieselregen", EN: "Light drizzle"},
	53: {Icon: "cloud-drizzle", DE: "Nieselregen", EN: "Moderate drizzle"},
	55: {Icon: "cloud-drizzle", DE: "Starker Nieselregen", EN: "Dense drizzle"},
	56: {Icon: "cloud-drizzle", DE: "Leichter gefrierender Nieselregen", EN: "Light freezing drizzle"},
	57: {Icon: "cloud-drizzle", DE: "Gefrierender Nieselregen", EN: "Dense freezing drizzle"},
	61: {Icon: "cloud-rain", DE: "Leichter Regen", EN: "Slight rain"},
	63: {Icon: "cloud-rain", DE: "Regen", EN: "Moderate rain"},
	65: {Icon: "cloud-rain", DE: "Starker Regen", EN: "Heavy rain"},
	66: {Icon: "cloud-rain", DE: "Leichter gefrierender Regen", EN: "Light freezing rain"},
	67: {Icon: "cloud-rain", DE: "Gefrierender Regen", EN: "Heavy freezing rain"},
	71: {Icon: "snowflake", DE: "Leichter Schneefall", EN: "Slight snow fall"},
	73: {Icon: "snowflake", DE: "Schneefall", EN: "Moderate snow fall"},
	75: {Icon: "snowflake", DE: "Starker Schneefall", EN: "Heavy snow fall"},
	77: {Icon: "snowflake", DE: "Schneegriesel", EN: "Snow grains"},
	80: {Icon: "cloud-rain-wind", DE: "Leichte Regenschauer", EN: "Slight rain showers"},
	81: {Icon: "cloud-rain-wind", DE: "Regenschauer", EN: "Moderate rain showers"},
	82: {Icon: "cloud-rain-wind", DE: "Heftige Regenschauer", EN: "Violent rain showers"},
	85: {Icon: "snowflake", DE: "Leichte Schneeschauer", EN: "Slight snow showers"},
	86: {Icon: "snowflake", DE: "Schneeschauer", EN: "Heavy snow showers"},
	95: {Icon: "cloud-lightning", DE: "Gewitter", EN: "Thunderstorm"},
	96: {Icon: "cloud-lightning", DE: "Gewitter mit leichtem Hagel", EN: "Thunderstorm w/ slight hail"},
	99: {Icon: "cloud-lightning", DE: "Gewitter mit Hagel", EN: "Thunderstorm w/ heavy hail"},
}

// languageEN identifies the English language code. resolveWeather
// returns the EN field for this exact value; every other lang
// (including empty) maps to the German default. Kept as a private
// constant so the choice of "what does empty mean" lives in one
// place.
const languageEN = "en"

// resolveWeather returns the icon and localized description for
// the given WMO code. lang is one of "de" / "en"; empty or any
// other value falls back to German. Codes outside the curated
// table return a defensive ("cloud", "Unbekannt"/"Unknown") pair
// so the screensaver does not crash on future WMO additions.
//
// The function is the single source of truth for both the Mieter-
// Web-Viewer and the /esp/* surfaces; both consumers go through
// weather.Client.Get, which calls resolveWeather right before
// returning the Snapshot.
func resolveWeather(code int, lang string) (icon, description string) {
	entry, ok := wmoTable[code]
	if !ok {
		if lang == languageEN {
			return "cloud", "Unknown"
		}
		return "cloud", "Unbekannt"
	}
	if lang == languageEN {
		return entry.Icon, entry.EN
	}
	return entry.Icon, entry.DE
}
