package stream

import (
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"

	"carvilon.local/stream/internal/profile"
	"carvilon.local/stream/internal/source"
	"carvilon.local/stream/internal/sourcereg"
)

// freeUDPPort returns a likely-free UDP port for the LAN ICE mux. There is a
// tiny TOCTOU window between close and the mux rebind, acceptable in a test.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free udp port: %v", err)
	}
	port := c.LocalAddr().(*net.UDPAddr).Port
	_ = c.Close()
	return port
}

// lanWHEPTestServer builds a *Server with the LAN-direct WHEP endpoint enabled
// on a free UDP port, a fake source, and the given egress key (nil -> open).
func lanWHEPTestServer(t *testing.T, egressKey []byte, profiles []profile.Profile) *Server {
	t.Helper()
	reg, err := profile.NewRegistry(profiles)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	factory := func(sourcereg.Key) (source.VideoSource, error) {
		return &fakeSource{frames: make(chan source.AccessUnit, 4)}, nil
	}
	srv, err := NewServer(ServerOptions{
		Profiles:       reg,
		SourceFactory:  factory,
		Addr:           ":0",
		Logger:         log.New(io.Discard, "", 0),
		LANWHEPICEPort: freeUDPPort(t),
		EgressHMACKey:  egressKey,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(srv.shutdownAll)
	return srv
}

func TestNewLANWHEPAPI(t *testing.T) {
	api, mux, err := newLANWHEPAPI(freeUDPPort(t))
	if err != nil {
		t.Fatalf("newLANWHEPAPI: %v", err)
	}
	if api == nil || mux == nil {
		t.Fatal("nil api or mux")
	}
	_ = mux.Close()
}

// TestEdgeWHEP_AuthGate: with an egress key configured, a WHEP request without
// a valid Bearer egress token is rejected 401 (fail-closed), BEFORE any track
// work - mirroring the cloud egress.
func TestEdgeWHEP_AuthGate(t *testing.T) {
	srv := lanWHEPTestServer(t, []byte("edge-egress-key"), []profile.Profile{
		passthroughProfile("intercom_web", "cam-1"),
	})
	cases := []struct {
		name string
		auth string
	}{
		{"no Authorization", ""},
		{"non-Bearer scheme", "Token abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/whep/intercom_web", strings.NewReader("dummy"))
			req.Header.Set("Content-Type", "application/sdp")
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			srv.srv.Handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}
}

// TestEdgeWHEP_RouteOffByDefault: with LANWHEPICEPort unset (0) the /whep route
// is not registered, so a POST falls through to the static file server (404),
// never 201/401. Proves the endpoint is off for the standalone/cloud paths.
func TestEdgeWHEP_RouteOffByDefault(t *testing.T) {
	reg, err := profile.NewRegistry([]profile.Profile{passthroughProfile("intercom_web", "cam-1")})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	srv, err := NewServer(ServerOptions{
		Profiles: reg,
		SourceFactory: func(sourcereg.Key) (source.VideoSource, error) {
			return &fakeSource{frames: make(chan source.AccessUnit, 4)}, nil
		},
		Addr:   ":0",
		Logger: log.New(io.Discard, "", 0),
		// LANWHEPICEPort: 0 -> off
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(srv.shutdownAll)
	if srv.whepAPI != nil {
		t.Error("whepAPI built despite LANWHEPICEPort=0")
	}
	req := httptest.NewRequest(http.MethodPost, "/whep/intercom_web", strings.NewReader("x"))
	rec := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusCreated || rec.Code == http.StatusUnauthorized {
		t.Errorf("status = %d; the /whep route should not be registered when off", rec.Code)
	}
}

// TestEdgeWHEP_AnswerAdvertisesLANHost is the core proof: with no egress key
// (open like /offer), a real WHEP subscribe yields a 201 whose answer carries
// a host candidate and NO srflx/relay - i.e. the SettingEngine advertises the
// real LAN host (no NAT1To1, no STUN/TURN), so ICE picks the direct LAN path.
func TestEdgeWHEP_AnswerAdvertisesLANHost(t *testing.T) {
	srv := lanWHEPTestServer(t, nil, []profile.Profile{
		passthroughProfile("intercom_web", "cam-1"),
	})

	// A minimal recvonly subscriber offer (default pion API).
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("subscriber pc: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatalf("transceiver: %v", err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local: %v", err)
	}
	<-gather

	req := httptest.NewRequest(http.MethodPost, "/whep/intercom_web", strings.NewReader(pc.LocalDescription().SDP))
	req.Header.Set("Content-Type", "application/sdp")
	rec := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	answer := rec.Body.String()
	if !strings.Contains(answer, "v=0") {
		t.Fatalf("answer is not SDP: %q", answer)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/whep/intercom_web/session/") {
		t.Errorf("Location = %q, want /whep/intercom_web/session/ prefix", loc)
	}
	// The whole point: LAN host candidate, NO relay/reflexive rewrite.
	if strings.Contains(answer, "typ srflx") || strings.Contains(answer, "typ relay") {
		t.Errorf("answer carries srflx/relay - LAN-direct config broken: %q", answer)
	}
	if !strings.Contains(answer, "typ host") {
		t.Errorf("answer carries no host candidate (private LAN IPv4 expected; if this host has only loopback, that explains it): %q", answer)
	}
}
