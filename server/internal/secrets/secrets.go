// Package secrets encrypts and decrypts platform secrets with
// AES-256-GCM. The 32-byte master key is read from the
// CARVILON_SECRETS_KEY environment variable as 64 hex characters
// (legacy alias: UNIFIX_SECRETS_KEY).
//
// The encrypted form is hex(nonce || ciphertext_with_tag).
// GCM appends a 16-byte authentication tag to the ciphertext,
// so a corrupted or tampered value fails decryption with an
// authentication error rather than silently yielding garbage.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
)

const (
	envKey       = "CARVILON_SECRETS_KEY"
	legacyEnvKey = "UNIFIX_SECRETS_KEY"
	keyByteLen   = 32
	keyHexLen    = keyByteLen * 2
	nonceLength  = 12 // GCM standard nonce size
)

// Sentinel errors.
var (
	ErrNoKey         = errors.New("secrets: CARVILON_SECRETS_KEY env var not set (legacy UNIFIX_SECRETS_KEY also accepted)")
	ErrInvalidKey    = errors.New("secrets: key must be 64 hex chars (32 bytes)")
	ErrDecryptFailed = errors.New("secrets: decrypt failed (wrong key or corrupted data)")
)

// Service holds the master key in memory. Construct once at
// server startup and pass to every package that needs to
// encrypt or decrypt.
type Service struct {
	key []byte
}

// New reads the secrets master-key env-var and parses it as 64
// hex characters. The canonical name is CARVILON_SECRETS_KEY;
// the legacy UNIFIX_SECRETS_KEY is still accepted as a Saison-14
// rename transition alias. Returns ErrNoKey if neither is set.
func New() (*Service, error) {
	raw := os.Getenv(envKey)
	if raw == "" {
		raw = os.Getenv(legacyEnvKey)
	}
	if raw == "" {
		return nil, ErrNoKey
	}
	if len(raw) != keyHexLen {
		return nil, ErrInvalidKey
	}
	key, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidKey, err)
	}
	return NewWithKey(key)
}

// NewWithKey lets tests inject a raw 32-byte key directly.
func NewWithKey(key []byte) (*Service, error) {
	if len(key) != keyByteLen {
		return nil, ErrInvalidKey
	}
	return &Service{key: key}, nil
}

// Encrypt encrypts plaintext with AES-256-GCM. Returns
// hex(nonce || ciphertext_with_tag).
func (s *Service) Encrypt(plaintext string) (string, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return "", fmt.Errorf("secrets: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("secrets: new gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("secrets: read nonce: %w", err)
	}
	ct := gcm.Seal(nil, nonce, []byte(plaintext), nil)
	out := append(nonce, ct...)
	return hex.EncodeToString(out), nil
}

// Decrypt reverses Encrypt. Returns ErrDecryptFailed on
// authentication failure (wrong key, tampered data) or on any
// other GCM error.
func (s *Service) Decrypt(encrypted string) (string, error) {
	raw, err := hex.DecodeString(encrypted)
	if err != nil {
		return "", fmt.Errorf("%w: hex decode: %v", ErrDecryptFailed, err)
	}
	if len(raw) < nonceLength {
		return "", ErrDecryptFailed
	}
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return "", fmt.Errorf("secrets: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("secrets: new gcm: %w", err)
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", ErrDecryptFailed
	}
	return string(pt), nil
}

// GenerateKeyHex produces a fresh 64-hex-char key from
// crypto/rand. Used by the cmd/genkey tool during initial
// server setup.
func GenerateKeyHex() (string, error) {
	b := make([]byte, keyByteLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
