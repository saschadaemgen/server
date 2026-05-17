// Saison 14-03-FIX02 Sub-1a: door-name-resolution coverage.
package httpserver

import "testing"

func TestResolveDoorName(t *testing.T) {
	doorMap := map[string]string{
		"28:70:4e:31:e2:9c": "Hauseingang",
		"aa:bb:cc:dd:ee:ff": "Tiefgarage",
	}
	cases := []struct {
		name string
		mac  string
		want string
	}{
		{
			name: "known intercom -> friendly door name",
			mac:  "28:70:4e:31:e2:9c",
			want: "Hauseingang",
		},
		{
			name: "second known intercom -> its own door name",
			mac:  "aa:bb:cc:dd:ee:ff",
			want: "Tiefgarage",
		},
		{
			name: "empty mac -> generic fallback",
			mac:  "",
			want: genericDoorName,
		},
		{
			name: "unknown intercom -> bare mac as last resort",
			mac:  "00:11:22:33:44:55",
			want: "00:11:22:33:44:55",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveDoorName(doorMap, tc.mac)
			if got != tc.want {
				t.Errorf("resolveDoorName(%q) = %q, want %q", tc.mac, got, tc.want)
			}
		})
	}
}

// Even with a nil-ish map (UA-API down so loadIntercomDoorNames
// returned an empty map), resolveDoorName must still produce a
// sensible label.
func TestResolveDoorName_EmptyMap(t *testing.T) {
	empty := map[string]string{}
	if got := resolveDoorName(empty, ""); got != genericDoorName {
		t.Errorf("empty mac with empty map: got %q, want %q", got, genericDoorName)
	}
	if got := resolveDoorName(empty, "28:70:4e:31:e2:9c"); got != "28:70:4e:31:e2:9c" {
		t.Errorf("known mac with empty map: got %q, want bare mac", got)
	}
}

// A map entry whose name is the empty string (UA returned a door
// without a name) should fall through to the MAC instead of
// rendering an empty label.
func TestResolveDoorName_BlankNameFallsThrough(t *testing.T) {
	m := map[string]string{"28:70:4e:31:e2:9c": ""}
	if got := resolveDoorName(m, "28:70:4e:31:e2:9c"); got != "28:70:4e:31:e2:9c" {
		t.Errorf("blank-name fallback: got %q, want bare mac", got)
	}
}
