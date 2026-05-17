// Saison 14-03-FIX02 + FIX03: door-name-resolution coverage.
package httpserver

import (
	"testing"

	"unifix.local/server/internal/uaapi"
)

// fakeDoor builds a uaapi.Door whose IntercomMAC() helper will
// return the colon-form lowercase MAC the test wants.
func fakeDoor(id, name, intercomBare string) uaapi.Door {
	thumb := ""
	if intercomBare != "" {
		thumb = "/preview/reader_" + intercomBare + "_" + id + "_1.jpg"
	}
	return uaapi.Door{
		ID:     id,
		Name:   name,
		Extras: uaapi.DoorExtras{DoorThumbnail: thumb},
	}
}

func TestResolveDoorName_MultiDoor(t *testing.T) {
	meta := doorMeta{
		intercomToName: map[string]string{
			"28:70:4e:31:e2:9c": "Hauseingang",
			"aa:bb:cc:dd:ee:ff": "Tiefgarage",
		},
		allDoors: []uaapi.Door{
			fakeDoor("d1", "Hauseingang", "28704e31e29c"),
			fakeDoor("d2", "Tiefgarage", "aabbccddeeff"),
		},
	}
	cases := []struct {
		name string
		mac  string
		want string
	}{
		{name: "known intercom -> friendly door name", mac: "28:70:4e:31:e2:9c", want: "Hauseingang"},
		{name: "second known intercom -> its own door name", mac: "aa:bb:cc:dd:ee:ff", want: "Tiefgarage"},
		{name: "empty mac in multi-door install -> generic", mac: "", want: genericDoorName},
		{name: "unknown intercom -> generic (no bare MAC)", mac: "00:11:22:33:44:55", want: genericDoorName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveDoorName(meta, tc.mac)
			if got != tc.want {
				t.Errorf("resolveDoorName(%q) = %q, want %q", tc.mac, got, tc.want)
			}
		})
	}
}

// FIX03 Sub-1b: in a single-door installation an empty intercom
// MAC (door_unlocked-style event) resolves to the only door's
// name instead of the generic "Tuer" label.
func TestResolveDoorName_SingleDoorFallback(t *testing.T) {
	meta := doorMeta{
		intercomToName: map[string]string{
			"28:70:4e:31:e2:9c": "Hauseingang",
		},
		allDoors: []uaapi.Door{
			fakeDoor("d1", "Hauseingang", "28704e31e29c"),
		},
	}
	if got := resolveDoorName(meta, ""); got != "Hauseingang" {
		t.Errorf("single-door empty-mac fallback: got %q, want Hauseingang", got)
	}
	if got := resolveDoorName(meta, "28:70:4e:31:e2:9c"); got != "Hauseingang" {
		t.Errorf("single-door known-mac: got %q, want Hauseingang", got)
	}
	if got := resolveDoorName(meta, "00:11:22:33:44:55"); got != genericDoorName {
		t.Errorf("single-door unknown-mac: got %q, want %q", got, genericDoorName)
	}
}

// UA-API down: empty meta produces only generic labels, never
// the bare MAC the pre-FIX03 resolver used to print.
func TestResolveDoorName_NoUAAvailable(t *testing.T) {
	empty := doorMeta{intercomToName: map[string]string{}}
	if got := resolveDoorName(empty, ""); got != genericDoorName {
		t.Errorf("empty mac, no doors: got %q, want %q", got, genericDoorName)
	}
	if got := resolveDoorName(empty, "28:70:4e:31:e2:9c"); got != genericDoorName {
		t.Errorf("known-mac, no doors: got %q, want %q (NOT the mac)", got, genericDoorName)
	}
}

// A map entry with an empty name should not surface as a blank
// label; resolveDoorName falls through to the generic label.
func TestResolveDoorName_BlankNameFallsThrough(t *testing.T) {
	meta := doorMeta{
		intercomToName: map[string]string{"28:70:4e:31:e2:9c": ""},
	}
	if got := resolveDoorName(meta, "28:70:4e:31:e2:9c"); got != genericDoorName {
		t.Errorf("blank-name fallback: got %q, want %q", got, genericDoorName)
	}
}
