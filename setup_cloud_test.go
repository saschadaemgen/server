package stream

import (
	"bytes"
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
