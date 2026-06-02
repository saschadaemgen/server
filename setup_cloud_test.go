package stream

import (
	"bytes"
	"net"
	"testing"
)

// cloudOpts returns construction-valid options. whip.New checks cert/key
// PRESENCE lazily (at serve time), so non-existent paths are fine for a
// pure construction test - we never listen here.
func cloudOpts() CloudSetupOptions {
	return CloudSetupOptions{
		Addr:     ":0",
		CertFile: "/nonexistent/cert.pem",
		KeyFile:  "/nonexistent/key.pem",
		HMACKey:  bytes.Repeat([]byte{0x2a}, 32),
	}
}

func TestSetupCloudInProcess_Happy(t *testing.T) {
	srv, shutdown, err := SetupCloudInProcess(cloudOpts())
	if err != nil {
		t.Fatalf("SetupCloudInProcess: %v", err)
	}
	if srv == nil {
		t.Fatal("nil CloudServer handle")
	}
	if shutdown == nil {
		t.Fatal("nil shutdown func")
	}
	// Idempotent shutdown (mirror of the edge ShutdownIdempotent test).
	if err := shutdown(); err != nil {
		t.Errorf("shutdown: %v", err)
	}
	if err := shutdown(); err != nil {
		t.Errorf("second shutdown should be a clean no-op: %v", err)
	}
}

func TestSetupCloudInProcess_RejectsEmptyHMAC(t *testing.T) {
	opts := cloudOpts()
	opts.HMACKey = nil
	if _, _, err := SetupCloudInProcess(opts); err == nil {
		t.Fatal("expected error for empty HMACKey (whip.New validation must surface)")
	}
}

func TestSetupCloudInProcess_RejectsMissingCert(t *testing.T) {
	opts := cloudOpts()
	opts.CertFile = ""
	if _, _, err := SetupCloudInProcess(opts); err == nil {
		t.Fatal("expected error for empty CertFile (whip.New validation must surface)")
	}
}

// TestSetupCloudInProcess_DefaultLogger proves a nil Logger is tolerated
// (mirrors the edge setup's nil-Logger default).
func TestSetupCloudInProcess_DefaultLogger(t *testing.T) {
	opts := cloudOpts()
	opts.Logger = nil
	srv, shutdown, err := SetupCloudInProcess(opts)
	if err != nil {
		t.Fatalf("SetupCloudInProcess with nil logger: %v", err)
	}
	if srv == nil || shutdown == nil {
		t.Fatal("nil handle/shutdown with default logger")
	}
}

// ephemeralUDPSeam returns a turnListenPacket seam that binds an ephemeral
// loopback port instead of the fixed 3478, so TURN tests never collide on
// a real port.
func ephemeralUDPSeam() func(network, address string) (net.PacketConn, error) {
	return func(network, _ string) (net.PacketConn, error) {
		return net.ListenPacket(network, "127.0.0.1:0")
	}
}

func TestSetupCloudInProcess_TURNDisabledByDefault(t *testing.T) {
	// cloudOpts() sets no TURNPublicIP -> TURN is soft-gated OFF; setup
	// still succeeds (WHIP/WHEP only, empty ICEServers).
	srv, shutdown, err := SetupCloudInProcess(cloudOpts())
	if err != nil {
		t.Fatalf("SetupCloudInProcess (TURN off): %v", err)
	}
	if srv == nil || shutdown == nil {
		t.Fatal("nil handle/shutdown")
	}
	_ = shutdown()
}

func TestSetupCloudInProcess_TURNEnabled(t *testing.T) {
	opts := cloudOpts()
	opts.TURNPublicIP = "203.0.113.9"
	opts.TURNSharedSecret = []byte("test-secret")
	opts.turnListenPacket = ephemeralUDPSeam()

	srv, shutdown, err := SetupCloudInProcess(opts)
	if err != nil {
		t.Fatalf("SetupCloudInProcess (TURN on): %v", err)
	}
	if srv == nil || shutdown == nil {
		t.Fatal("nil handle/shutdown")
	}
	// Idempotent shutdown also closes the TURN relay exactly once.
	if err := shutdown(); err != nil {
		t.Errorf("shutdown: %v", err)
	}
	if err := shutdown(); err != nil {
		t.Errorf("second shutdown should be a clean no-op: %v", err)
	}
}

func TestSetupCloudInProcess_TURNInvalidIP(t *testing.T) {
	opts := cloudOpts()
	opts.TURNPublicIP = "not-an-ip"
	opts.TURNSharedSecret = []byte("test-secret")
	opts.turnListenPacket = ephemeralUDPSeam()
	if _, _, err := SetupCloudInProcess(opts); err == nil {
		t.Fatal("expected error for invalid TURNPublicIP")
	}
}

func TestSetupCloudInProcess_TURNMissingSecret(t *testing.T) {
	opts := cloudOpts()
	opts.TURNPublicIP = "203.0.113.9"
	opts.TURNSharedSecret = nil
	opts.turnListenPacket = ephemeralUDPSeam()
	if _, _, err := SetupCloudInProcess(opts); err == nil {
		t.Fatal("expected error for TURN enabled without shared secret")
	}
}

// ephemeralTLSSeam returns a turnListenTLS seam binding an ephemeral
// loopback TCP listener - no real cert/port - for the TLS-leg
// construction test. Production uses LoadX509KeyPair + tls.Listen.
func ephemeralTLSSeam() func(address string) (net.Listener, error) {
	return func(string) (net.Listener, error) {
		return net.Listen("tcp", "127.0.0.1:0")
	}
}

func TestSetupCloudInProcess_TURNWithTLS(t *testing.T) {
	opts := cloudOpts()
	opts.TURNPublicIP = "203.0.113.9"
	opts.TURNSharedSecret = []byte("test-secret")
	opts.TURNTLSPort = 5349
	opts.turnListenPacket = ephemeralUDPSeam()
	opts.turnListenTLS = ephemeralTLSSeam()

	srv, shutdown, err := SetupCloudInProcess(opts)
	if err != nil {
		t.Fatalf("SetupCloudInProcess (TURN + TLS): %v", err)
	}
	if srv == nil || shutdown == nil {
		t.Fatal("nil handle/shutdown")
	}
	if err := shutdown(); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

func TestSetupCloudInProcess_TURNTLSBadCert(t *testing.T) {
	// TLS on, no TLS seam -> real LoadX509KeyPair on cloudOpts()'s
	// non-existent cert paths -> hard error (no silent partial relay).
	opts := cloudOpts()
	opts.TURNPublicIP = "203.0.113.9"
	opts.TURNSharedSecret = []byte("test-secret")
	opts.TURNTLSPort = 5349
	opts.turnListenPacket = ephemeralUDPSeam()
	// turnListenTLS deliberately unset -> exercises the eager cert load.
	if _, _, err := SetupCloudInProcess(opts); err == nil {
		t.Fatal("expected error for TLS-on with an unloadable cert")
	}
}
