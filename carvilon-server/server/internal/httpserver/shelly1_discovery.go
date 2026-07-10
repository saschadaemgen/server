// Gen1 mDNS discovery - the classifier side. Gen1 Shellies do not
// advertise the Gen2+ _shelly._tcp service; they announce a plain
// _http._tcp instance named "<model>-<deviceid>" ("shelly1-B929CC",
// "shellyswitch25-A4CF12E4B7C1"). Browsing _http._tcp hears EVERY web
// thing on the LAN, so admission is a strict allowlist of the documented
// Gen1 relay-class name prefixes - never a "looks shelly-ish" heuristic:
//
//   - a Gen2+ device also announces _http._tcp ("shellyplus1pm-...",
//     "shellypro4pm-..."); those prefixes are NOT in the list, so the
//     same physical device cannot enter once per generation;
//   - non-Shelly devices never match at all;
//   - out-of-scope Gen1 models (dimmers, RGBW, battery sensors) are
//     deliberately absent - deferred capability work, not silently
//     admitted with a wrong shape.
//
// Classification is announcement-only: like the Gen2 path, a pending
// device is NEVER contacted before the operator approves it. The 3-byte
// device-id tails of longid=0 firmwares are not a usable MAC (the store
// requires the full 12 hex digits), so such finds are keyed by address
// until provisioning learns the full MAC from GET /shelly.
package httpserver

import (
	"strings"

	"carvilon.local/server/internal/dnssd"
	"carvilon.local/server/internal/shellystore"
)

// Shelly1ServiceType is the DNS-SD service Gen1 Shelly devices announce
// on. Exported so main can open the second dnssd browser for it.
const Shelly1ServiceType = "_http._tcp.local"

// gen1NamePrefixes maps the documented Gen1 mDNS/MQTT instance-name
// prefixes of the relay-class scope to their type codes. Matching is
// prefix + "-" (the id separator), longest prefix first, so "shelly1pm-"
// never falls into the "shelly1" bucket and "shellyplug-s-" never into
// "shellyplug". Order: see gen1PrefixOrder.
var gen1NamePrefixes = map[string]string{
	"shellyswitch25": "SHSW-25",
	"shellyplug-s":   "SHPLG-S",
	"shellyswitch":   "SHSW-21",
	"shellyplug-u1":  "SHPLG-U1",
	"shelly1pm":      "SHSW-PM",
	"shellyplug":     "SHPLG-1",
	"shelly1l":       "SHSW-L",
	"shelly1":        "SHSW-1",
}

// gen1PrefixOrder is the longest-first match order for gen1NamePrefixes.
var gen1PrefixOrder = []string{
	"shellyswitch25",
	"shellyplug-u1",
	"shellyplug-s",
	"shellyswitch",
	"shellyplug",
	"shelly1pm",
	"shelly1l",
	"shelly1",
}

// shelly1DetectedFromEntry turns a _http._tcp announcement into the
// store's Detected shape - or ok=false for anything that is not an
// in-scope Gen1 Shelly. It applies the same address trust rule as the
// Gen2 path (RFC1918 only, canonical dial form) and is pure (no I/O), so
// the policy is unit-tested directly.
func shelly1DetectedFromEntry(e dnssd.Entry) (shellystore.Detected, bool) {
	label := strings.ToLower(strings.TrimSpace(e.InstanceLabel()))
	typeCode := ""
	for _, p := range gen1PrefixOrder {
		if strings.HasPrefix(label, p+"-") {
			typeCode = gen1NamePrefixes[p]
			break
		}
	}
	if typeCode == "" {
		return shellystore.Detected{}, false
	}
	addr, ok := shellyAddrFromEntry(e)
	if !ok {
		return shellystore.Detected{}, false
	}
	return shellystore.Detected{
		// The tail is a full 12-hex MAC only on longid=1 firmwares; a
		// 3-byte tail yields "" and the device is keyed by address.
		MAC:     macFromShellyID(label),
		Address: addr,
		Name:    e.InstanceLabel(),
		Model:   typeCode,
		Gen:     shellystore.Gen1,
	}, true
}
