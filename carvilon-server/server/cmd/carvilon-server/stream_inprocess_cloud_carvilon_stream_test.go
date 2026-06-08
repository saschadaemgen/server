//go:build carvilon_stream

package main

import (
	"strings"
	"testing"
	"time"

	"carvilon.local/stream"

	"carvilon.local/server/internal/config"
)

// TestToTurnstoreEvent_DropsRawIP proves the VPS-side conversion keeps
// only the masked address. The raw IP must never appear in the carvilon
// event that crosses the side-channel (Sascha's privacy rule).
func TestToTurnstoreEvent_DropsRawIP(t *testing.T) {
	ok := true
	in := stream.TURNEvent{
		Kind:          "auth",
		Time:          time.Now(),
		SrcAddr:       "203.0.113.5:54321",
		SrcAddrMasked: "v4:203.0.x.x#ab12",
		DstAddr:       "198.51.100.9:7000",
		DstAddrMasked: "v4:198.51.x.x#cd34",
		Protocol:      "udp",
		Username:      "carvilon",
		Realm:         "carvilon",
		AuthOK:        &ok,
	}
	out := toTurnstoreEvent(in)

	if out.SrcMasked != in.SrcAddrMasked || out.DstMasked != in.DstAddrMasked {
		t.Fatalf("masked addresses not carried: src=%q dst=%q", out.SrcMasked, out.DstMasked)
	}
	if out.AuthOK == nil || !*out.AuthOK {
		t.Fatalf("AuthOK not carried: %v", out.AuthOK)
	}
	// No raw IP may appear anywhere in the converted event.
	hay := out.SrcMasked + out.DstMasked + out.Username + out.Realm + out.Err + out.Protocol + out.Kind
	for _, raw := range []string{"203.0.113.5", "198.51.100.9", "54321", "7000"} {
		if strings.Contains(hay, raw) {
			t.Fatalf("raw IP/port %q leaked into the converted event", raw)
		}
	}
}

// TestBuildTURNSnapshot_DropsRawClientIP proves the snapshot client
// list carries only the masked address, and the config view is filled
// from cfg when TURN is configured.
func TestBuildTURNSnapshot_DropsRawClientIP(t *testing.T) {
	st := stream.TURNStats{
		Enabled:         true,
		AllocationCount: 1,
		Clients: []stream.TURNClient{{
			SrcAddr:       "203.0.113.5:54321",
			SrcAddrMasked: "v4:203.0.x.x#ab12",
			Username:      "carvilon",
			Since:         time.Now(),
		}},
	}
	cfg := config.Config{
		TURNPublicIP:     "203.0.113.5",
		TURNSharedSecret: "shhh",
		TURNUDPPort:      3478,
		TURNTLSPort:      5349,
		TURNRealm:        "carvilon",
		TURNPublicHost:   "turn.carvilon.com",
		TURNTLSCertFile:  "/etc/letsencrypt/live/turn.carvilon.com/fullchain.pem",
	}
	snap := buildTURNSnapshot(st, cfg)

	if len(snap.Clients) != 1 || snap.Clients[0].SrcMasked != "v4:203.0.x.x#ab12" {
		t.Fatalf("masked client not carried: %+v", snap.Clients)
	}
	if strings.Contains(snap.Clients[0].SrcMasked+snap.Clients[0].Username, "54321") {
		t.Fatalf("raw client port leaked into snapshot")
	}
	if !snap.Enabled || snap.UDPPort != 3478 || snap.TLSPort != 5349 || snap.TURNSHost != "turn.carvilon.com" {
		t.Fatalf("config view not filled: %+v", snap)
	}
	if snap.CertMode != "separate" {
		t.Fatalf("CertMode = %q, want separate (own TLS cert set)", snap.CertMode)
	}
	if !snap.STUNActive {
		t.Fatalf("STUNActive should track the enabled relay")
	}
}
