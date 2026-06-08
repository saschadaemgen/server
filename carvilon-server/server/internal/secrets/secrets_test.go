package secrets

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, keyByteLen)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	s, err := NewWithKey(testKey(t))
	if err != nil {
		t.Fatalf("NewWithKey: %v", err)
	}
	plain := "supersecret-ua-api-token-value-12345"
	enc, err := s.Encrypt(plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if enc == plain {
		t.Fatal("encrypted value equals plaintext")
	}
	if _, err := hex.DecodeString(enc); err != nil {
		t.Errorf("encrypted is not hex: %v", err)
	}
	got, err := s.Decrypt(enc)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != plain {
		t.Errorf("Decrypt = %q, want %q", got, plain)
	}
}

func TestEncrypt_FreshNoncePerCall(t *testing.T) {
	s, err := NewWithKey(testKey(t))
	if err != nil {
		t.Fatalf("NewWithKey: %v", err)
	}
	a, _ := s.Encrypt("same")
	b, _ := s.Encrypt("same")
	if a == b {
		t.Error("two encryptions of the same plaintext produced identical ciphertext (nonce reused)")
	}
}

func TestDecrypt_WrongKey(t *testing.T) {
	a, err := NewWithKey(testKey(t))
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	enc, err := a.Encrypt("secret")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	other := make([]byte, keyByteLen)
	for i := range other {
		other[i] = byte(255 - i)
	}
	b, err := NewWithKey(other)
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	_, err = b.Decrypt(enc)
	if !errors.Is(err, ErrDecryptFailed) {
		t.Errorf("Decrypt with wrong key = %v, want ErrDecryptFailed", err)
	}
}

func TestDecrypt_CorruptedCiphertext(t *testing.T) {
	s, err := NewWithKey(testKey(t))
	if err != nil {
		t.Fatalf("NewWithKey: %v", err)
	}
	enc, _ := s.Encrypt("secret")
	tampered := []byte(enc)
	// flip a hex char in the middle (ciphertext area, not nonce)
	mid := len(tampered) - 4
	if tampered[mid] == 'a' {
		tampered[mid] = 'b'
	} else {
		tampered[mid] = 'a'
	}
	_, err = s.Decrypt(string(tampered))
	if !errors.Is(err, ErrDecryptFailed) {
		t.Errorf("Decrypt of tampered ct = %v, want ErrDecryptFailed", err)
	}
}

func TestDecrypt_NotHex(t *testing.T) {
	s, err := NewWithKey(testKey(t))
	if err != nil {
		t.Fatalf("NewWithKey: %v", err)
	}
	_, err = s.Decrypt("not-a-hex-string")
	if !errors.Is(err, ErrDecryptFailed) {
		t.Errorf("Decrypt non-hex = %v, want ErrDecryptFailed", err)
	}
}

func TestDecrypt_TooShort(t *testing.T) {
	s, err := NewWithKey(testKey(t))
	if err != nil {
		t.Fatalf("NewWithKey: %v", err)
	}
	_, err = s.Decrypt("aabb")
	if !errors.Is(err, ErrDecryptFailed) {
		t.Errorf("Decrypt too-short = %v, want ErrDecryptFailed", err)
	}
}

func TestNew_NoEnvVar(t *testing.T) {
	t.Setenv(envKey, "")
	_, err := New()
	if !errors.Is(err, ErrNoKey) {
		t.Errorf("New with no env = %v, want ErrNoKey", err)
	}
}

func TestNew_InvalidKeyLength(t *testing.T) {
	t.Setenv(envKey, "0123")
	_, err := New()
	if !errors.Is(err, ErrInvalidKey) {
		t.Errorf("New with short key = %v, want ErrInvalidKey", err)
	}
}

func TestNew_InvalidKeyChars(t *testing.T) {
	t.Setenv(envKey, strings.Repeat("z", 64))
	_, err := New()
	if !errors.Is(err, ErrInvalidKey) {
		t.Errorf("New with non-hex chars = %v, want ErrInvalidKey", err)
	}
}

func TestNewWithKey_RejectsWrongLength(t *testing.T) {
	_, err := NewWithKey([]byte{1, 2, 3})
	if !errors.Is(err, ErrInvalidKey) {
		t.Errorf("NewWithKey short = %v, want ErrInvalidKey", err)
	}
}

func TestGenerateKeyHex_Format(t *testing.T) {
	k, err := GenerateKeyHex()
	if err != nil {
		t.Fatalf("GenerateKeyHex: %v", err)
	}
	if len(k) != keyHexLen {
		t.Errorf("key length = %d, want %d", len(k), keyHexLen)
	}
	if _, err := hex.DecodeString(k); err != nil {
		t.Errorf("key not hex: %v", err)
	}
	// fresh key each call
	k2, _ := GenerateKeyHex()
	if k == k2 {
		t.Error("two GenerateKeyHex calls produced identical output")
	}
}

func TestDeriveSubkey_DeterministicAndNamespaced(t *testing.T) {
	s, err := NewWithKey(testKey(t))
	if err != nil {
		t.Fatalf("NewWithKey: %v", err)
	}
	a := s.DeriveSubkey("publish-token")
	if len(a) != 32 {
		t.Fatalf("subkey length = %d, want 32", len(a))
	}
	// Deterministic: same label yields the same subkey (so issued
	// tokens survive a restart).
	if hex.EncodeToString(a) != hex.EncodeToString(s.DeriveSubkey("publish-token")) {
		t.Error("DeriveSubkey not deterministic for the same label")
	}
	// Namespaced: a different label yields a different subkey.
	if hex.EncodeToString(a) == hex.EncodeToString(s.DeriveSubkey("other-purpose")) {
		t.Error("DeriveSubkey returned identical keys for different labels")
	}
	// Never the master key verbatim.
	if hex.EncodeToString(a) == hex.EncodeToString(testKey(t)) {
		t.Error("DeriveSubkey returned the master key verbatim")
	}
}
