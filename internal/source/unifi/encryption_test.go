package unifi

import (
	"strings"
	"testing"
)

// --- stripEnableSrtp ---------------------------------------------------------

func TestStripEnableSrtp_OnlyParam(t *testing.T) {
	in := "rtsps://192.168.1.1:7441/abc?enableSrtp"
	got := stripEnableSrtp(in)
	want := "rtsps://192.168.1.1:7441/abc"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripEnableSrtp_WithOtherParams(t *testing.T) {
	in := "rtsps://192.168.1.1:7441/abc?enableSrtp&foo=bar&baz=1"
	got := stripEnableSrtp(in)
	// url.Values.Encode() sorts keys alphabetically. We accept either
	// order — what matters is that enableSrtp is gone and the other two
	// remain intact.
	for _, want := range []string{"foo=bar", "baz=1"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in %q", want, got)
		}
	}
	if strings.Contains(got, "enableSrtp") {
		t.Errorf("enableSrtp not stripped: %q", got)
	}
	if !strings.HasPrefix(got, "rtsps://192.168.1.1:7441/abc?") {
		t.Errorf("unexpected prefix in %q", got)
	}
}

func TestStripEnableSrtp_NoQueryAtAll(t *testing.T) {
	in := "rtsps://192.168.1.1:7441/abc"
	got := stripEnableSrtp(in)
	if got != in {
		t.Errorf("URL without query should be unchanged; got %q", got)
	}
}

func TestStripEnableSrtp_NoEnableSrtpButOtherParams(t *testing.T) {
	in := "rtsps://192.168.1.1:7441/abc?token=xyz"
	got := stripEnableSrtp(in)
	if got != in {
		t.Errorf("URL without enableSrtp should be unchanged; got %q want %q", got, in)
	}
}

func TestStripEnableSrtp_PreservesPath(t *testing.T) {
	// A real UniFi feed-id can be a long opaque token-bearing path. The
	// stripper must not touch the path or its escaping.
	in := "rtsps://192.168.1.1:7441/abc:def_ghi.JKL%2FMNO?enableSrtp"
	got := stripEnableSrtp(in)
	want := "rtsps://192.168.1.1:7441/abc:def_ghi.JKL%2FMNO"
	if got != want {
		t.Errorf("path mangled; got %q, want %q", got, want)
	}
}

func TestStripEnableSrtp_UnparseableReturnsInput(t *testing.T) {
	// Pathological input — should not panic, should not lose the value.
	in := "::not a url::"
	got := stripEnableSrtp(in)
	if got != in {
		t.Errorf("unparseable input must be returned unchanged; got %q", got)
	}
}

// --- NewSource encryption mode handling --------------------------------------

func TestNewSource_EncryptionDefaultsToTLS(t *testing.T) {
	src, err := NewSource(Options{
		NVRHost:  "host",
		APIKey:   "k",
		CameraID: "c",
	})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if src.opts.Encryption != EncryptionTLS {
		t.Errorf("default Encryption = %q, want %q", src.opts.Encryption, EncryptionTLS)
	}
}

func TestNewSource_EncryptionTLSExplicit(t *testing.T) {
	src, err := NewSource(Options{
		NVRHost:    "host",
		APIKey:     "k",
		CameraID:   "c",
		Encryption: EncryptionTLS,
	})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if src.opts.Encryption != EncryptionTLS {
		t.Errorf("Encryption = %q, want %q", src.opts.Encryption, EncryptionTLS)
	}
}

func TestNewSource_EncryptionSRTPAccepted(t *testing.T) {
	// S6-11: SRTP is now implemented (SDES, not MIKEY — see srtp.go).
	// NewSource must ACCEPT the value; the actual SRTP key derivation
	// happens at Start time once the SDP is on hand. This test would
	// previously have asserted that NewSource ERRORED with the sentinel.
	src, err := NewSource(Options{
		NVRHost:    "host",
		APIKey:     "k",
		CameraID:   "c",
		Encryption: EncryptionSRTP,
	})
	if err != nil {
		t.Fatalf("NewSource(EncryptionSRTP) errored: %v (S6-11: should accept)", err)
	}
	if src.opts.Encryption != EncryptionSRTP {
		t.Errorf("Encryption = %q, want %q", src.opts.Encryption, EncryptionSRTP)
	}
	// The SRTP receiver is NOT armed yet — that happens in Start
	// (needs the SDP). The field must be nil until then.
	if src.srtp != nil {
		t.Error("SRTP receiver was armed at construction; should only happen in Start after DESCRIBE")
	}
}

func TestNewSource_EncryptionUnknownRejected(t *testing.T) {
	_, err := NewSource(Options{
		NVRHost:    "host",
		APIKey:     "k",
		CameraID:   "c",
		Encryption: "rot13",
	})
	if err == nil {
		t.Fatal("expected error for unknown encryption value")
	}
	if !strings.Contains(err.Error(), "rot13") {
		t.Errorf("error should mention the bad value, got %v", err)
	}
}
