package esptoken

import (
	"testing"
)

func TestGenerate_ReturnsUniqueTokens(t *testing.T) {
	a, ah, err := Generate()
	if err != nil {
		t.Fatalf("Generate a: %v", err)
	}
	b, bh, err := Generate()
	if err != nil {
		t.Fatalf("Generate b: %v", err)
	}
	if a == b {
		t.Error("two consecutive tokens equal (RNG broken?)")
	}
	if ah == bh {
		t.Error("two consecutive hashes equal")
	}
	if len(a) < 40 {
		t.Errorf("clear text length = %d, want >= 40 (base64url-encoded 32 bytes)", len(a))
	}
	if len(ah) != 64 {
		t.Errorf("hash length = %d, want 64 (sha256 hex)", len(ah))
	}
}

func TestVerify_HappyPath(t *testing.T) {
	clear, hash, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !Verify(clear, hash) {
		t.Error("Verify returned false for valid pair")
	}
}

func TestVerify_WrongToken(t *testing.T) {
	_, hash, _ := Generate()
	if Verify("not-the-real-token", hash) {
		t.Error("Verify accepted wrong token")
	}
}

func TestVerify_EmptyArgs(t *testing.T) {
	clear, hash, _ := Generate()
	if Verify("", hash) {
		t.Error("Verify accepted empty token")
	}
	if Verify(clear, "") {
		t.Error("Verify accepted empty hash")
	}
}

func TestPreview(t *testing.T) {
	if got := Preview("abcdefgh1234"); got != "abcdefgh..." {
		t.Errorf("Preview long = %q, want abcdefgh...", got)
	}
	if got := Preview("short"); got != "short" {
		t.Errorf("Preview short = %q, want short", got)
	}
	if got := Preview(""); got != "" {
		t.Errorf("Preview empty = %q, want empty", got)
	}
}
