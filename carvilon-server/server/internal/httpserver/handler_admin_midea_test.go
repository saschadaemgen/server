package httpserver

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"carvilon.local/server/internal/db"
	"carvilon.local/server/internal/mideaclimate"
	"carvilon.local/server/internal/mideastore"
	"carvilon.local/server/internal/secrets"
)

// TestAdminUA_MideaOnlyRendersTable guards the gate-card regression: a
// Midea-only deployment (UA off, no Shelly/Protect/RPi) must render the device
// table + the unified Scan-network button + the adopt dialog (region US
// default), NOT the "UniFi Access is disabled" gate card.
func TestAdminUA_MideaOnlyRendersTable(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	d, err := db.Open(filepath.Join(t.TempDir(), "midea.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	sec, err := secrets.NewWithKey(make([]byte, 32))
	if err != nil {
		t.Fatalf("secrets.NewWithKey: %v", err)
	}
	store := mideastore.New(d.DB, sec)
	if _, err := store.InsertDiscovered(context.Background(), mideastore.Detected{
		DeviceID: 0xABCDEF, Address: "192.0.2.50", Name: "net_ac_test", ProtocolV3: true,
	}); err != nil {
		t.Fatalf("InsertDiscovered: %v", err)
	}
	env.srv.mideastore = store

	body := getBody(t, env, "/a/devices")
	if strings.Contains(body, `class="dc-gate`) {
		t.Errorf("gate card shown although Midea is enabled (should render the table)")
	}
	for _, want := range []string{
		"data-dcscan-btn",          // unified scan button present
		"net_ac_test",              // the pending device row
		"/a/devices/midea/approve", // adopt form
		`value="US" selected`,      // region default US
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Midea-only render missing %q", want)
		}
	}
}

func TestMideaDiscoverableIP(t *testing.T) {
	cases := map[string]bool{
		"192.168.1.50":    true,  // private
		"10.0.0.9":        true,  // private
		"172.16.5.5":      true,  // private
		"127.0.0.1":       true,  // loopback (dev stub)
		"8.8.8.8":         false, // public
		"169.254.169.254": false, // link-local / IMDS
		"169.254.1.1":     false, // link-local
		"not-an-ip":       false,
		"":                false,
	}
	for ip, want := range cases {
		if got := mideaDiscoverableIP(ip); got != want {
			t.Errorf("mideaDiscoverableIP(%q) = %v, want %v", ip, got, want)
		}
	}
}

func TestClassifyMideaPairError(t *testing.T) {
	fetch := func(inner string) error {
		return fmt.Errorf("%w: %w", mideaclimate.ErrCredentialFetch, errors.New(inner))
	}
	handshake := fmt.Errorf("%w: %w", mideaclimate.ErrLocalHandshake, errors.New("timeout"))

	// Realistic strings from the corrected cloud.go (wrapped by pairing.Pair).
	cases := []struct {
		name      string
		err       error
		wantFlash string
	}{
		{"handshake", handshake, "midea-pair-handshake"},
		{"getLoginID fail", fetch("[Region US] Anmeldung fehlgeschlagen (getLoginID): getLoginID: keine loginId in der Antwort"), "midea-pair-cloud-login"},
		{"login no session", fetch("[Region US] Anmeldung fehlgeschlagen (getLoginID -> login): login: keine sessionId in der Antwort"), "midea-pair-cloud-login"},
		{"token/region", fetch("[Region US] Anmeldung OK, aber kein Token/Key fuer Geraet 5 gefunden. Letzter Fehler: getToken: keine tokenlist in der Antwort"), "midea-pair-token"},
		{"cloud api during login", fetch("[Region US] Anmeldung fehlgeschlagen (getLoginID): getLoginID: Cloud-Fehler 3102: sign illegal"), "midea-pair-cloud-api"},
		{"import", fetch("keine importierten Credentials fuer Geraet 5"), "midea-pair-import"},
		{"unknown fetch", fetch("something odd"), "midea-pair-err"},
		{"unclassified", errors.New("totally generic"), "midea-pair-err"},
	}
	for _, c := range cases {
		got := classifyMideaPairError(c.err, "DE")
		if got.flash != c.wantFlash {
			t.Errorf("%s: flash = %q, want %q (label %q)", c.name, got.flash, c.wantFlash, got.stepLabel)
		}
		if got.stepLabel == "" || got.detail == "" {
			t.Errorf("%s: empty stepLabel/detail", c.name)
		}
	}
	// The region is woven into the token/region hint.
	tokErr := fetch("kein Token/Key fuer Geraet 5 gefunden")
	if d := classifyMideaPairError(tokErr, "KR"); !strings.Contains(d.detail, "KR") {
		t.Errorf("token detail should mention region KR, got %q", d.detail)
	}
}
