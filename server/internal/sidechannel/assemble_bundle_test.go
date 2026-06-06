package sidechannel

import (
	"net"
	"testing"
)

// TestAssembleBundle_CarriesEdgeWHEPURL: the cloud assembler puts the
// edge-supplied LAN-direct URL into the bundle when present, omits it when
// empty, and leaves the cloud-side fields unchanged. (Saison 19-41, Fall B)
func TestAssembleBundle_CarriesEdgeWHEPURL(t *testing.T) {
	p := genCerts(t, t.TempDir())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	srv, err := NewServer(ServerOptions{
		Listener:   ln,
		CACertPath: p.caCrt,
		ServerCert: p.serverCrt,
		ServerKey:  p.serverKey,
		Log:        quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.SetWHEPBaseURL("https://whep.example")

	const mac = "0c:ea:14:00:00:01"
	// 192.0.2.x is the RFC 5737 documentation range (placeholder).
	const edge = "http://192.0.2.10:8555/whep/intercom_android"

	// Edge supplied a URL -> present in the bundle; cloud fields intact.
	b := srv.AssembleBundle(mac, "tok-x", 300, edge)
	if b.EdgeWHEPURL != edge {
		t.Errorf("EdgeWHEPURL = %q, want %q", b.EdgeWHEPURL, edge)
	}
	if b.WHEPURL != "https://whep.example/whep/"+mac {
		t.Errorf("WHEPURL = %q, want cloud base + /whep/<mac>", b.WHEPURL)
	}
	if b.StreamID != mac || b.EgressToken != "tok-x" || b.ExpiresIn != 300 {
		t.Errorf("bundle core fields changed: %+v", b)
	}

	// No edge URL -> EdgeWHEPURL empty (omitempty drops it from the JSON);
	// the cloud fields are unchanged.
	b2 := srv.AssembleBundle(mac, "tok-x", 300, "")
	if b2.EdgeWHEPURL != "" {
		t.Errorf("EdgeWHEPURL = %q, want empty", b2.EdgeWHEPURL)
	}
	if b2.WHEPURL != "https://whep.example/whep/"+mac {
		t.Errorf("WHEPURL changed when edge URL empty: %q", b2.WHEPURL)
	}
}
