// Saison 14-03-FIX02 + FIX03: door-name-resolution coverage.
package httpserver

import (
	"testing"

	"carvilon.local/server/internal/uaapi"
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

// FIX04 Sub-1b: in a single-door installation the only door is
// the answer regardless of the row's intercom field - including
// the unknown-MAC case (was generic "Tuer" pre-FIX04). A
// single-door site has exactly one candidate; calling it "Tuer"
// when we know its real name is silly.
func TestResolveDoorName_SingleDoorWinsOverUnknownIntercom(t *testing.T) {
	meta := doorMeta{
		intercomToName: map[string]string{
			"28:70:4e:31:e2:9c": "Hauseingang",
		},
		allDoors: []uaapi.Door{
			fakeDoor("d1", "Hauseingang", "28704e31e29c"),
		},
	}
	cases := []struct{ name, mac, want string }{
		{name: "empty mac in single-door install", mac: "", want: "Hauseingang"},
		{name: "known mac in single-door install", mac: "28:70:4e:31:e2:9c", want: "Hauseingang"},
		{name: "unknown mac in single-door install", mac: "00:11:22:33:44:55", want: "Hauseingang"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveDoorName(meta, tc.mac); got != tc.want {
				t.Errorf("resolveDoorName(%q) = %q, want %q", tc.mac, got, tc.want)
			}
		})
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

// FIX04 Sub-1a: doorShortName must prefer the short Name over
// the hierarchical FullName. uaapi.Door's own DisplayName helper
// stays unchanged (admin-side callers still get full_name) but
// every mieter-facing render goes through doorShortName.
func TestDoorShortName_PreferenceOrder(t *testing.T) {
	cases := []struct {
		name string
		d    uaapi.Door
		want string
	}{
		{
			name: "both set: Name wins over FullName",
			d:    uaapi.Door{ID: "d1", Name: "Hauseingangstür", FullName: "UDM SE - 1F - Hauseingangstür"},
			want: "Hauseingangstür",
		},
		{
			name: "only FullName: fall back to FullName",
			d:    uaapi.Door{ID: "d1", FullName: "UDM SE - 1F - Hauseingangstür"},
			want: "UDM SE - 1F - Hauseingangstür",
		},
		{
			name: "neither: fall back to ID",
			d:    uaapi.Door{ID: "d1"},
			want: "d1",
		},
		{
			name: "blank name with FullName: FullName wins",
			d:    uaapi.Door{ID: "d1", Name: "", FullName: "UDM SE - Backyard"},
			want: "UDM SE - Backyard",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := doorShortName(tc.d); got != tc.want {
				t.Errorf("doorShortName = %q, want %q", got, tc.want)
			}
		})
	}
}

// FIX04 Sub-1c: the generic fallback label must carry a real
// umlaut. Documents the convention so a future refactor that
// reaches for the ASCII spelling fails the test.
func TestGenericDoorName_HasUmlaut(t *testing.T) {
	if genericDoorName != "Tür" {
		t.Errorf("genericDoorName = %q, want %q", genericDoorName, "Tür")
	}
}
