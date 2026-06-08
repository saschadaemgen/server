// Package argon2id wraps golang.org/x/crypto/argon2 with the PHC
// string format and OWASP-2024-recommended parameters (m=64MB,
// t=3, p=4). Argon2id replaced bcrypt as the platform's password
// hash; the admin login migrates legacy bcrypt hashes on the
// first successful verify (rehash-on-login).
//
// Pepper: every hash is concatenated with a 32-byte pepper stored
// in platform_config (AES-256-GCM-encrypted), so a stolen SQLite
// file cannot be brute-forced offline without the server-side
// context.
package argon2id

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// OWASP-2024 parameters for Argon2id (see Cheat Sheet).
// 64 MiB memory, 3 iterations, 4 parallel lanes; stays under
// 250ms even on an RPi 4.
const (
	Memory      uint32 = 64 * 1024
	Iterations  uint32 = 3
	Parallelism uint8  = 4
	SaltLength         = 16
	KeyLength   uint32 = 32
)

// ErrInvalidHash flags a hash string that is not in the PHC
// format or declares an unknown algorithm.
var ErrInvalidHash = errors.New("argon2id: invalid hash format")

// HashWithPepper hashes password+pepper and returns the PHC string:
//
//	$argon2id$v=19$m=65536,t=3,p=4$<base64-salt>$<base64-hash>
//
// An empty pepper is allowed (e.g. for tests that run without
// platform_config); the result is then plain Argon2id without the
// pepper-concatenation step.
func HashWithPepper(password, pepper string) (string, error) {
	salt := make([]byte, SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("argon2id: read salt: %w", err)
	}
	peppered := []byte(password + pepper)
	hash := argon2.IDKey(peppered, salt, Iterations, Memory, Parallelism, KeyLength)

	b64Salt := base64.RawStdEncoding.EncodeToString(salt)
	b64Hash := base64.RawStdEncoding.EncodeToString(hash)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, Memory, Iterations, Parallelism, b64Salt, b64Hash), nil
}

// VerifyWithPepper checks password+pepper against a PHC string.
// Constant-time compare via crypto/subtle.
func VerifyWithPepper(password, pepper, encodedHash string) (bool, error) {
	params, salt, hash, err := decodeHash(encodedHash)
	if err != nil {
		return false, err
	}
	peppered := []byte(password + pepper)
	other := argon2.IDKey(peppered, salt, params.iterations, params.memory,
		params.parallelism, uint32(len(hash)))
	if subtle.ConstantTimeCompare(hash, other) == 1 {
		return true, nil
	}
	return false, nil
}

// LooksLikeArgon2id is a cheap check whether a stored hash string
// is in the Argon2id PHC format. The admin login uses it to
// decide whether a bcrypt hash is present and needs rehashing.
func LooksLikeArgon2id(s string) bool {
	return strings.HasPrefix(s, "$argon2id$")
}

type params struct {
	memory      uint32
	iterations  uint32
	parallelism uint8
}

func decodeHash(encodedHash string) (params, []byte, []byte, error) {
	parts := strings.Split(encodedHash, "$")
	// PHC: ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, hash]
	if len(parts) != 6 {
		return params{}, nil, nil, ErrInvalidHash
	}
	if parts[1] != "argon2id" {
		return params{}, nil, nil, ErrInvalidHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return params{}, nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return params{}, nil, nil, ErrInvalidHash
	}
	var p params
	for _, kv := range strings.Split(parts[3], ",") {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			return params{}, nil, nil, ErrInvalidHash
		}
		key, val := kv[:eq], kv[eq+1:]
		n, err := strconv.ParseUint(val, 10, 32)
		if err != nil {
			return params{}, nil, nil, ErrInvalidHash
		}
		switch key {
		case "m":
			p.memory = uint32(n)
		case "t":
			p.iterations = uint32(n)
		case "p":
			if n > 255 {
				return params{}, nil, nil, ErrInvalidHash
			}
			p.parallelism = uint8(n)
		default:
			return params{}, nil, nil, ErrInvalidHash
		}
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return params{}, nil, nil, ErrInvalidHash
	}
	hash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return params{}, nil, nil, ErrInvalidHash
	}
	return p, salt, hash, nil
}
