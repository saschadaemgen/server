package unifi

import (
	"encoding/base64"
	"strings"
	"testing"
)

// The point of these tests is not to prove SDP parsing — it is to prove
// that the secret-bearing fields (inline keys, MIKEY payload bytes) NEVER
// make it into the summary string. That summary string is what we log.

func TestSDPSecurityReport_NoCrypto_NoKeyMgmt(t *testing.T) {
	sdp := []byte("v=0\r\n" +
		"o=- 0 0 IN IP4 127.0.0.1\r\n" +
		"s=stream\r\n" +
		"m=video 0 RTP/AVP 97\r\n" +
		"a=rtpmap:97 H264/90000\r\n" +
		"a=control:trackID=0\r\n")
	got := sdpSecurityReport(sdp)
	if !strings.Contains(got, "a=crypto count=0") {
		t.Errorf("expected crypto count=0, got %q", got)
	}
	if !strings.Contains(got, "a=key-mgmt count=0") {
		t.Errorf("expected key-mgmt count=0, got %q", got)
	}
	if !strings.Contains(got, "RTP/AVP") {
		t.Errorf("expected m-line RTP/AVP to appear, got %q", got)
	}
}

func TestSDPSecurityReport_CryptoInlineKeyRedacted(t *testing.T) {
	// Real-shape SDES line. The "key bytes" here are the secret.
	keyBytes := make([]byte, 30) // 16 master key + 14 master salt
	for i := range keyBytes {
		keyBytes[i] = byte(0xA0 + i)
	}
	keyB64 := base64.StdEncoding.EncodeToString(keyBytes)

	sdp := []byte("v=0\r\n" +
		"m=video 0 RTP/SAVP 97\r\n" +
		"a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:" + keyB64 + "|2^31|1:1\r\n")

	got := sdpSecurityReport(sdp)

	// PRIMARY ASSERTION: the inline key MUST NOT appear verbatim.
	if strings.Contains(got, keyB64) {
		t.Fatalf("inline base64 key leaked into report:\n%s", got)
	}
	// Also reject any 8-char substring of the key, in case we accidentally
	// log a prefix or suffix.
	for i := 0; i+12 <= len(keyB64); i += 4 {
		fragment := keyB64[i : i+12]
		if strings.Contains(got, fragment) {
			t.Fatalf("inline base64 key fragment %q leaked into report:\n%s", fragment, got)
		}
	}

	// Diagnostic fields we DO want present.
	for _, want := range []string{
		"a=crypto count=1",
		"tag=1",
		"suite=AES_CM_128_HMAC_SHA1_80",
		"inline=<redacted 30B>",
		"params=2^31|1:1",
		"RTP/SAVP",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in report, not found in:\n%s", want, got)
		}
	}
}

func TestSDPSecurityReport_KeyMgmtMikeyPayloadRedacted(t *testing.T) {
	payload := []byte("super-secret-MIKEY-blob-bytes-go-here")
	payloadB64 := base64.StdEncoding.EncodeToString(payload)

	sdp := []byte("v=0\r\n" +
		"a=key-mgmt:mikey " + payloadB64 + "\r\n" +
		"m=video 0 RTP/SAVP 97\r\n")

	got := sdpSecurityReport(sdp)

	if strings.Contains(got, payloadB64) {
		t.Fatalf("MIKEY payload leaked into report:\n%s", got)
	}
	if strings.Contains(got, "super-secret") {
		t.Fatalf("MIKEY plaintext leaked into report:\n%s", got)
	}

	for _, want := range []string{
		"a=key-mgmt count=1",
		"method=mikey",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in report:\n%s", want, got)
		}
	}
}

func TestSDPSecurityReport_OtherAttrsNamesOnly(t *testing.T) {
	sdp := []byte("v=0\r\n" +
		"m=video 0 RTP/AVP 97\r\n" +
		"a=rtpmap:97 H264/90000\r\n" +
		"a=fmtp:97 packetization-mode=1;sprop-parameter-sets=SECRET_LOOKING_BUT_NOT_SECRET\r\n" +
		"a=control:trackID=0\r\n" +
		"a=range:npt=0-\r\n")

	got := sdpSecurityReport(sdp)

	// Names should appear; values must not.
	if !strings.Contains(got, "rtpmap") || !strings.Contains(got, "fmtp") || !strings.Contains(got, "control") {
		t.Errorf("expected attribute names in report, got %q", got)
	}
	if strings.Contains(got, "SECRET_LOOKING_BUT_NOT_SECRET") {
		t.Errorf("attribute value leaked into report:\n%s", got)
	}
	if strings.Contains(got, "trackID=0") {
		t.Errorf("attribute value leaked into report:\n%s", got)
	}
}

func TestSummarizeCryptoValue_NonInlineMethodRedacted(t *testing.T) {
	// Some implementations carry a non-"inline" key-method (e.g. "uri:")
	// where the value would also be sensitive.
	got := summarizeCryptoValue("1 AES_CM_128_HMAC_SHA1_80 uri:https://example.invalid/secret-key-fetch")
	if strings.Contains(got, "example.invalid") || strings.Contains(got, "secret-key-fetch") {
		t.Fatalf("non-inline key URI leaked: %s", got)
	}
	if !strings.Contains(got, "uri=<redacted>") {
		t.Errorf("expected 'uri=<redacted>' in summary, got %q", got)
	}
}
