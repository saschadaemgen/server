// Saison 14-03-FIX02: shared helpers that resolve an intercom MAC
// (the only thing door_events stores per row) into a human door
// name (what the mieter actually wants to read in the history
// list).
//
// Both the synchronous /webviewer home render and the JSON
// /webviewer/history.json endpoint render up to ViewerHistoryLimit
// events at once. Looking each row up via the UA-API would
// produce ViewerHistoryLimit round-trips; instead we call
// uaapi.ListDoors EXACTLY once per request, build an in-memory
// map from intercom MAC to door name, and let each row resolve
// against the map.
//
// If UA is unavailable (no client wired, or ListDoors errors)
// the map is empty and rows fall through to "Hauseingang"
// (intercom MAC unknown for that row) or the bare MAC string
// (last resort). The full MAC only appears when UA is completely
// down AND the intercom MAC is known - in normal operation
// every row carries a friendly door name.
package httpserver

import "context"

// genericDoorName is the fallback label for history rows that
// have no intercom MAC (early test data, manual DB inserts).
const genericDoorName = "Hauseingang"

// loadIntercomDoorNames returns a map of lowercase colon-form
// intercom MAC -> human door name, built from a single
// ListDoors call. Empty map (not nil) on error so callers can
// resolve without nil-checking.
func (s *Server) loadIntercomDoorNames(ctx context.Context) map[string]string {
	if s.ua == nil {
		return map[string]string{}
	}
	doors, err := s.ua.ListDoors(ctx)
	if err != nil {
		s.log.Warn("door-name resolution: ua list-doors failed",
			"err", err)
		return map[string]string{}
	}
	m := make(map[string]string, len(doors))
	for _, d := range doors {
		mac := d.IntercomMAC()
		if mac == "" {
			continue
		}
		m[mac] = d.DisplayName()
	}
	return m
}

// resolveDoorName picks the best label for a history row's
// intercom MAC. Order:
//
//  1. UA-API door name from the prefetched map
//  2. "Hauseingang" when the row has no intercom MAC
//     (we have nothing specific to point to)
//  3. The bare MAC as a last resort - shown only when the
//     intercom MAC IS known but UA-API has no matching door
//     (UA down, or door not bound to this intercom).
func resolveDoorName(doorMap map[string]string, intercomMAC string) string {
	if intercomMAC == "" {
		return genericDoorName
	}
	if name, ok := doorMap[intercomMAC]; ok && name != "" {
		return name
	}
	return intercomMAC
}
