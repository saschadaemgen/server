// Saison 14-03-FIX02 + FIX03: shared helpers that resolve an
// intercom MAC (the only thing door_events stores per row) into
// a human door name (what the mieter actually wants to read in
// the history list).
//
// Both the synchronous /webviewer home render and the JSON
// /webviewer/history.json endpoint render up to ViewerHistoryLimit
// events at once. Looking each row up via the UA-API would
// produce ViewerHistoryLimit round-trips; instead we call
// uaapi.ListDoors EXACTLY once per request, build doorMeta
// (intercom-MAC map + the full door list), and let every row
// resolve against the cached snapshot.
//
// FIX03 Sub-1b: door_events does not yet carry a door_id column
// (TODO for a later saison schema change), so door_unlocked
// rows triggered via the developer-API often have an empty
// intercom_mac. For those rows resolveDoorName falls back to
// the single existing door's name when the installation has
// exactly one door; multi-door setups get a generic "Tuer"
// label and will need the schema-level door_id once a customer
// actually has more than one door per viewer.
//
// The bare MAC is NEVER returned anymore - the FIX02 last-
// resort "show the MAC" path turned out to confuse users more
// than it informed them (they cannot map an arbitrary
// 12-hex string to a real door anyway). Unknown known-MAC
// rows now also get the generic "Tuer" label.
package httpserver

import (
	"context"

	"carvilon.local/server/internal/uaapi"
)

// genericDoorName is the fallback label for history rows that
// have no intercom MAC and live in a multi-door installation
// (single-door installs use the actual door name).
//
// FIX04 Sub-1c: real "ue" umlaut in the user-facing string;
// ASCII spelling stays in comments and log fields only.
const genericDoorName = "Tür"

// doorMeta is the cached snapshot used by resolveDoorName. The
// map covers the typical intercom-routed case; allDoors carries
// the full list so the empty-intercom branch can pick a single-
// door fallback.
type doorMeta struct {
	intercomToName map[string]string
	allDoors       []uaapi.Door
}

// loadDoorMeta makes the one UA-API round-trip per render and
// builds both views. Empty/zero on error so callers do not
// need to nil-check.
//
// Replaces the FIX02-era loadIntercomDoorNames helper.
func (s *Server) loadDoorMeta(ctx context.Context) doorMeta {
	if s.ua == nil {
		return doorMeta{intercomToName: map[string]string{}}
	}
	doors, err := s.ua.ListDoors(ctx)
	if err != nil {
		s.log.Warn("door-name resolution: ua list-doors failed",
			"err", err)
		return doorMeta{intercomToName: map[string]string{}}
	}
	m := make(map[string]string, len(doors))
	for _, d := range doors {
		mac := d.IntercomMAC()
		if mac == "" {
			continue
		}
		m[mac] = doorShortName(d)
	}
	return doorMeta{intercomToName: m, allDoors: doors}
}

// doorShortName picks the user-facing label for a uaapi.Door,
// preferring the short `name` over the hierarchical `full_name`.
// FIX02 used uaapi.DisplayName() which inverted the priority and
// surfaced UA's "<Hub-Device> - <Floor> - <Door>" path strings
// to the mieter. uaapi.DisplayName stays unchanged for admin
// callers that actually want the full path.
//
// Saison 14-03-FIX04 Sub-1a.
func doorShortName(d uaapi.Door) string {
	if d.Name != "" {
		return d.Name
	}
	if d.FullName != "" {
		return d.FullName
	}
	return d.ID
}

// resolveDoorName picks the best label for a history row.
// Order (FIX04 Sub-1b):
//
//  1. Single-door installation: the one door is the answer
//     regardless of the row's intercom field. Covers BOTH the
//     empty-intercom case (door_unlocked via developer-API)
//     AND the unknown-intercom case (UA-API can no longer
//     resolve the MAC for some reason). FIX03 used the generic
//     "Tuer" label in the unknown-intercom-with-single-door
//     case which is silly when we know there's exactly one
//     candidate.
//  2. Known intercom MAC -> the mapped door name (typical case
//     for doorbell_start, where the intercom MAC was captured
//     in the /remote_view RPC).
//  3. Anything else (multi-door + unknown/empty intercom MAC)
//     -> generic label. Multi-door installs need the schema-
//     level door_id to do better; until then the generic label
//     is honest.
func resolveDoorName(meta doorMeta, intercomMAC string) string {
	if len(meta.allDoors) == 1 {
		if name := doorShortName(meta.allDoors[0]); name != "" {
			return name
		}
	}
	if intercomMAC != "" {
		if name, ok := meta.intercomToName[intercomMAC]; ok && name != "" {
			return name
		}
	}
	return genericDoorName
}
