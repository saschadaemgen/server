package unifi

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// TestSRTPKDF_RFC3711AppendixB3 is THE belt-and-braces test. RFC 3711
// publishes the AES-CM KDF output for a specific master_key /
// master_salt under labels 0x00 (encryption key), 0x01 (auth key),
// 0x02 (session salt). If our KDF reproduces those bit-for-bit, the
// rest of the SRTP transform is on solid ground — every later
// derivation in our pipeline is just AES-CM keystream + XOR + HMAC.
//
// Vectors lifted verbatim from RFC 3711 §B.3 and cross-checked
// against the verified Python reference in
// C:\Projects\UniFi\tools\mikey_crack\8_manual_srtp.py.
func TestSRTPKDF_RFC3711AppendixB3(t *testing.T) {
	masterKey := mustHex(t, "E1F97A0D3E018BE0D64FA32C06DE4139")
	masterSalt := mustHex(t, "0EC675AD498AFEEBB6960B3AABE6")

	cases := []struct {
		name   string
		label  byte
		outLen int
		want   string
	}{
		{"session encryption key (label 0)", 0x00, 16, "C61E7A93744F39EE10734AFE3FF7A087"},
		{"session authentication key (label 1)", 0x01, 20, "CEBE321F6FF7716B6FD4AB49AF256A156D38BAA4"},
		{"session salt (label 2)", 0x02, 14, "30CBBC08863D8C85D49DB34A9AE1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := srtpKDF(masterKey, masterSalt, c.label, c.outLen)
			if err != nil {
				t.Fatalf("srtpKDF: %v", err)
			}
			want := mustHex(t, c.want)
			if !bytes.Equal(got, want) {
				t.Errorf("label %#x:\n got:  %s\n want: %s",
					c.label, hex.EncodeToString(got), hex.EncodeToString(want))
			}
		})
	}
}

// TestSRTPKDF_RejectsBadKeySize asserts the size guards on the KDF —
// callers passing the wrong-size buffer should fail loudly, not
// silently produce garbage keys.
func TestSRTPKDF_RejectsBadKeySize(t *testing.T) {
	salt := make([]byte, 14)
	if _, err := srtpKDF(make([]byte, 15), salt, 0, 16); err == nil {
		t.Error("expected error for 15-byte master key")
	}
	if _, err := srtpKDF(make([]byte, 16), make([]byte, 13), 0, 16); err == nil {
		t.Error("expected error for 13-byte master salt")
	}
}

// TestSRTPReceiver_ROCWrap is the synthetic-overflow test the briefing
// requires. The 16-bit RTP sequence wraps every 65 536 packets; the
// SRTP packet index is 48-bit, with ROC providing the upper 32. We
// MUST detect the wrap and bump ROC, otherwise the IV (and the HMAC)
// kick into wrong territory ~40 min in at 25 fps and the stream goes
// silent.
//
// Construction: feed sequence numbers around two distinct wrap
// points, with intermediate ordered traffic between them, and assert
// the ROC value updateROC returns for each call.
func TestSRTPReceiver_ROCWrap(t *testing.T) {
	r := &srtpReceiver{}

	// Pre-wrap: 65000 → 65500 → 65535. ROC stays 0.
	for _, seq := range []uint16{65000, 65500, 65535} {
		if roc := r.updateROC(seq); roc != 0 {
			t.Errorf("pre-wrap seq=%d: roc=%d, want 0", seq, roc)
		}
	}
	// First wrap: 65535 → 0. ROC becomes 1.
	if roc := r.updateROC(0); roc != 1 {
		t.Errorf("first wrap seq=0: roc=%d, want 1", roc)
	}
	// Post-wrap ordered traffic: 0 → 100 → 30000. ROC stays 1.
	for _, seq := range []uint16{100, 30000} {
		if roc := r.updateROC(seq); roc != 1 {
			t.Errorf("post-wrap seq=%d: roc=%d, want 1", seq, roc)
		}
	}
	// Approach next wrap: 60000 → 65535.
	for _, seq := range []uint16{60000, 65535} {
		if roc := r.updateROC(seq); roc != 1 {
			t.Errorf("approach 2nd wrap seq=%d: roc=%d, want 1", seq, roc)
		}
	}
	// Second wrap: 65535 → 5. ROC becomes 2.
	if roc := r.updateROC(5); roc != 2 {
		t.Errorf("second wrap seq=5: roc=%d, want 2", roc)
	}
}

// TestSRTPReceiver_NoWrapOnSmallBackwardJump: a forward stream with a
// small backwards step (e.g. a delayed packet) MUST NOT trigger a ROC
// increment — only the >0x8000 backwards jump counts as a wrap.
func TestSRTPReceiver_NoWrapOnSmallBackwardJump(t *testing.T) {
	r := &srtpReceiver{}
	if got := r.updateROC(1000); got != 0 {
		t.Errorf("initial seq=1000: roc=%d, want 0", got)
	}
	// "Reordering" by ~100 seqs: still roc=0.
	if got := r.updateROC(900); got != 0 {
		t.Errorf("backwards <0x8000 seq=900: roc=%d, want 0", got)
	}
	// Forward again.
	if got := r.updateROC(1100); got != 0 {
		t.Errorf("forward seq=1100: roc=%d, want 0", got)
	}
}

// TestSRTPProcess_RoundTrip is the end-to-end correctness test:
// encrypt + tag a packet using the SAME crypto primitives the
// receiver decrypts with, then ask the receiver to recover the
// plaintext. If both halves are wrong in the same direction we'd
// pass — TestSRTPKDF_RFC3711AppendixB3 nails the KDF independently
// against RFC, so a passing round-trip then implies the per-packet
// IV / XOR / HMAC pipeline is correct.
func TestSRTPProcess_RoundTrip(t *testing.T) {
	masterKey := mustHex(t, "E1F97A0D3E018BE0D64FA32C06DE4139")
	masterSalt := mustHex(t, "0EC675AD498AFEEBB6960B3AABE6")

	rx, err := newSRTPReceiver(masterKey, masterSalt)
	if err != nil {
		t.Fatalf("newSRTPReceiver: %v", err)
	}

	plaintext := []byte("hello SRTP — this is a fake H.264 payload")
	// Build a synthetic RTP packet: v=2, no padding, no extension,
	// no CSRC, marker=0, PT=97, seq=42, ts=0xDEADBEEF, ssrc=0xCAFEBABE.
	header := []byte{
		0x80, 97, 0, 42,
		0xDE, 0xAD, 0xBE, 0xEF,
		0xCA, 0xFE, 0xBA, 0xBE,
	}
	wire := encryptAndTagForTest(t, header, plaintext, rx.sessionKey, rx.authKey, rx.sessionSalt, 0)

	got, err := rx.process(wire)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("round-trip mismatch:\n got:  %x\n want: %x", got, plaintext)
	}
}

// TestSRTPProcess_AuthFailureRejected: a packet with a corrupted tag
// MUST return ErrSRTPAuth. The receiver must not return a "decrypted"
// payload from an unauthenticated packet.
func TestSRTPProcess_AuthFailureRejected(t *testing.T) {
	masterKey := mustHex(t, "E1F97A0D3E018BE0D64FA32C06DE4139")
	masterSalt := mustHex(t, "0EC675AD498AFEEBB6960B3AABE6")

	rx, err := newSRTPReceiver(masterKey, masterSalt)
	if err != nil {
		t.Fatalf("newSRTPReceiver: %v", err)
	}
	header := []byte{0x80, 97, 0, 42, 0xDE, 0xAD, 0xBE, 0xEF, 0xCA, 0xFE, 0xBA, 0xBE}
	wire := encryptAndTagForTest(t, header, []byte("payload"),
		rx.sessionKey, rx.authKey, rx.sessionSalt, 0)
	// Flip a bit in the auth tag.
	wire[len(wire)-1] ^= 0x01

	_, err = rx.process(wire)
	if !errors.Is(err, ErrSRTPAuth) {
		t.Errorf("got err=%v, want ErrSRTPAuth", err)
	}
}

// TestSRTPProcess_RoundTripAcrossWrap: encrypt+verify packets that
// straddle a sequence wrap, asserting the receiver applies the
// matching ROC value to both IV and HMAC of each packet.
func TestSRTPProcess_RoundTripAcrossWrap(t *testing.T) {
	masterKey := mustHex(t, "E1F97A0D3E018BE0D64FA32C06DE4139")
	masterSalt := mustHex(t, "0EC675AD498AFEEBB6960B3AABE6")
	rx, err := newSRTPReceiver(masterKey, masterSalt)
	if err != nil {
		t.Fatalf("newSRTPReceiver: %v", err)
	}

	header := func(seq uint16) []byte {
		h := []byte{0x80, 97, 0, 0, 0xDE, 0xAD, 0xBE, 0xEF, 0xCA, 0xFE, 0xBA, 0xBE}
		binary.BigEndian.PutUint16(h[2:4], seq)
		return h
	}

	// Pre-wrap: seq 65500 with ROC=0.
	plain1 := []byte("frame at seq 65500")
	wire1 := encryptAndTagForTest(t, header(65500), plain1, rx.sessionKey, rx.authKey, rx.sessionSalt, 0)
	got1, err := rx.process(wire1)
	if err != nil {
		t.Fatalf("pre-wrap process: %v", err)
	}
	if !bytes.Equal(got1, plain1) {
		t.Errorf("pre-wrap payload mismatch")
	}

	// Post-wrap: seq 100 with ROC=1 (because the receiver should
	// have bumped after the wrap).
	plain2 := []byte("frame at seq 100, post-wrap")
	wire2 := encryptAndTagForTest(t, header(100), plain2, rx.sessionKey, rx.authKey, rx.sessionSalt, 1)
	got2, err := rx.process(wire2)
	if err != nil {
		t.Fatalf("post-wrap process: %v", err)
	}
	if !bytes.Equal(got2, plain2) {
		t.Errorf("post-wrap payload mismatch")
	}
}

// --- SDES key extraction ---------------------------------------------------

func TestExtractSDESVideoKey_PicksVideoOnly(t *testing.T) {
	// Realistic shape: three m= sections, only video has a crypto.
	// 30 bytes of secret encoded base64 ⇒ 40 base64 chars.
	const audioKeyB64 = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="     // 32 B (deliberately wrong size for audio)
	const videoKeyB64 = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGx0d"           // 30 B
	sdp := strings.Join([]string{
		"v=0",
		"m=audio 0 RTP/AVP 100",
		"a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:" + audioKeyB64 + "|2^31",
		"m=application 0 RTP/AVP 101",
		"m=video 0 RTP/AVP 97",
		"a=rtpmap:97 H264/90000",
		"a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:" + videoKeyB64 + "|2^31|1:1",
		"",
	}, "\r\n")

	got, err := extractSDESVideoKey([]byte(sdp))
	if err != nil {
		t.Fatalf("extractSDESVideoKey: %v", err)
	}
	if len(got) != 30 {
		t.Fatalf("got %d-byte key, want 30", len(got))
	}
	// Should have picked the video key, not the audio key.
	wantPrefix := []byte{0x00, 0x01, 0x02, 0x03}
	if !bytes.HasPrefix(got, wantPrefix) {
		t.Errorf("got prefix %x, want %x (audio key bled through?)", got[:4], wantPrefix)
	}
}

func TestExtractSDESVideoKey_NoVideoCrypto(t *testing.T) {
	sdp := "v=0\r\nm=video 0 RTP/AVP 97\r\na=rtpmap:97 H264/90000\r\n"
	_, err := extractSDESVideoKey([]byte(sdp))
	if err == nil {
		t.Error("expected error for SDP without a=crypto in video section")
	}
}

func TestExtractSDESVideoKey_RejectsWrongSize(t *testing.T) {
	// Base64-decoded payload is 16 bytes — not the required 30.
	const tooShort = "AAECAwQFBgcICQoLDA0ODw=="
	sdp := "v=0\r\nm=video 0 RTP/AVP 97\r\na=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:" + tooShort + "|2^31\r\n"
	_, err := extractSDESVideoKey([]byte(sdp))
	if err == nil {
		t.Error("expected error for 16-byte SDES inline (not 30)")
	}
}

// --- internal helpers ------------------------------------------------------

// encryptAndTagForTest builds a SRTP-on-the-wire packet from a
// cleartext header + payload, using the same primitives the receiver
// uses for decryption (round-trip-symmetric). Used by the round-trip
// tests above. Lives in _test.go so production code never grows an
// "encrypt" path it doesn't need.
func encryptAndTagForTest(t *testing.T, header, plaintext, sessionKey, authKey, sessionSalt []byte, roc uint32) []byte {
	t.Helper()
	if len(header) < 12 {
		t.Fatalf("test header too short: %d", len(header))
	}
	seq := binary.BigEndian.Uint16(header[2:4])
	ssrc := header[8:12]

	// Build the per-packet IV (same recipe as srtpReceiver.process).
	iv := make([]byte, 16)
	copy(iv, sessionSalt)
	iv[4] ^= ssrc[0]
	iv[5] ^= ssrc[1]
	iv[6] ^= ssrc[2]
	iv[7] ^= ssrc[3]
	pktIndex := (uint64(roc) << 16) | uint64(seq)
	iv[8] ^= byte(pktIndex >> 40)
	iv[9] ^= byte(pktIndex >> 32)
	iv[10] ^= byte(pktIndex >> 24)
	iv[11] ^= byte(pktIndex >> 16)
	iv[12] ^= byte(pktIndex >> 8)
	iv[13] ^= byte(pktIndex)

	block, err := aes.NewCipher(sessionKey)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	ciphertext := make([]byte, len(plaintext))
	cipher.NewCTR(block, iv).XORKeyStream(ciphertext, plaintext)

	pre := append([]byte{}, header...)
	pre = append(pre, ciphertext...)
	mac := hmac.New(sha1.New, authKey)
	mac.Write(pre)
	var rocBE [4]byte
	binary.BigEndian.PutUint32(rocBE[:], roc)
	mac.Write(rocBE[:])
	tag := mac.Sum(nil)[:srtpAuthTagLen]

	return append(pre, tag...)
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex.DecodeString: %v", err)
	}
	return b
}
