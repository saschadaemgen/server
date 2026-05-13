// Package argon2id wraps golang.org/x/crypto/argon2 with the PHC
// string format and OWASP-2024-empfohlenen Parametern (m=64MB,
// t=3, p=4). Saison 13-02-FIX4-a fuehrt Argon2id als Ersatz fuer
// bcrypt ein; der Admin-Login migriert beim ersten Argon2id-
// Verify den vorhandenen bcrypt-Hash automatisch (Rehash-on-Login).
//
// Pepper: alle Hashes werden mit einem in platform_config
// (AES-256-GCM-verschluesselt) abgelegten 32-Byte-Pepper
// konkateniert, damit ein gestohlenes SQLite-File ohne Server-
// Kontext nicht offline brute-forced werden kann.
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

// OWASP-2024-Parameter fuer Argon2id (siehe Cheat Sheet).
// 64 MiB Speicher, 3 Iterationen, 4 paralleler Lanes; das ist
// auch auf einem RPi 4 unter 250ms.
const (
	Memory      uint32 = 64 * 1024
	Iterations  uint32 = 3
	Parallelism uint8  = 4
	SaltLength         = 16
	KeyLength   uint32 = 32
)

// ErrInvalidHash flaggt einen Hash-String, der nicht das PHC-
// Format hat oder einen unbekannten Algorithmus deklariert.
var ErrInvalidHash = errors.New("argon2id: invalid hash format")

// HashWithPepper hasht password+pepper und liefert den PHC-String:
//
//	$argon2id$v=19$m=65536,t=3,p=4$<base64-salt>$<base64-hash>
//
// Pepper darf leer sein (z.B. in Tests die ohne platform_config
// auskommen); das ist explizit erlaubt, das Verfahren bleibt
// dann nur die Standard-Argon2id-Variante ohne Pepper.
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

// VerifyWithPepper prueft password+pepper gegen einen PHC-String.
// Constant-time-Vergleich via crypto/subtle.
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

// LooksLikeArgon2id ist ein billiger Test ob ein gespeicherter
// Hash-String im Argon2id-PHC-Format ist. Der Admin-Login nutzt
// das um zu entscheiden ob ein Bcrypt-Hash vorliegt der re-hashed
// werden soll.
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
