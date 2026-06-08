package httpserver

import (
	"strings"
	"testing"

	"carvilon.local/server/internal/config"
)

// TestEdgeWHEPURL covers the S19-35 LAN-direct switch: edge_whep_url is built
// (with the PROFILE name, not the MAC - Flagge A) only when the LAN-WHEP is
// active AND the edge LAN IP is known, and is omitted otherwise.
func TestEdgeWHEPURL(t *testing.T) {
	// 192.0.2.x is the RFC 5737 documentation range - a placeholder, never
	// a real LAN IP.
	active := config.Config{
		ServerIPv4:           "192.0.2.10",
		StreamAddr:           ":8555",
		StreamLANWHEPICEPort: 8556,
	}

	t.Run("active carries the profile, not the mac", func(t *testing.T) {
		got, ok := EdgeWHEPURL(active, "intercom_android")
		if !ok {
			t.Fatal("ok=false, want true (LAN-WHEP active + IP known)")
		}
		want := "http://192.0.2.10:8555/whep/intercom_android"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		if !strings.HasSuffix(got, "/whep/intercom_android") {
			t.Errorf("edge URL %q must end with the PROFILE name (Flagge A: not the MAC)", got)
		}
	})

	t.Run("port unset disables", func(t *testing.T) {
		c := active
		c.StreamLANWHEPICEPort = 0
		if got, ok := EdgeWHEPURL(c, "intercom_android"); ok || got != "" {
			t.Errorf("port=0 -> (%q,%v), want (\"\",false)", got, ok)
		}
	})

	t.Run("unknown edge ip disables", func(t *testing.T) {
		c := active
		c.ServerIPv4 = ""
		if got, ok := EdgeWHEPURL(c, "intercom_android"); ok || got != "" {
			t.Errorf("no ip -> (%q,%v), want (\"\",false)", got, ok)
		}
	})

	t.Run("empty profile disables", func(t *testing.T) {
		if got, ok := EdgeWHEPURL(active, ""); ok || got != "" {
			t.Errorf("empty profile -> (%q,%v), want (\"\",false)", got, ok)
		}
	})

	t.Run("unparseable stream addr disables", func(t *testing.T) {
		c := active
		c.StreamAddr = ""
		if got, ok := EdgeWHEPURL(c, "intercom_android"); ok || got != "" {
			t.Errorf("empty StreamAddr -> (%q,%v), want (\"\",false)", got, ok)
		}
	})
}
