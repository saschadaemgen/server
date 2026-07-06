// HTTP digest authentication (RFC 7616) for the Gen2 RPC endpoint.
// Protected Shelly devices answer 401 with a Digest challenge whose
// algorithm is SHA-256 and whose username is fixed to "admin"; the
// standard library has no digest client, so this file implements the
// one exchange the client needs: parse the challenge, compute the
// response hash, emit the Authorization header for a single retry.
// SHA-256 ONLY: a real Gen2+ device never offers anything else, so
// an MD5 (or absent) algorithm can only come from an impostor
// answering on the device's address - refusing it means no weaker
// password-derived material ever leaves this client.
package shellyapi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// digestAuthorization turns a WWW-Authenticate challenge into the
// matching Authorization header value for one request. It supports
// qop="auth" (the Gen2 form) and the legacy no-qop response; auth-int
// is not offered by the devices and not implemented.
func digestAuthorization(challenge, username, password, method, uri string) (string, error) {
	scheme, params := splitChallenge(challenge)
	if !strings.EqualFold(scheme, "Digest") {
		return "", errors.New("not a digest challenge")
	}
	realm, hasRealm := params["realm"]
	nonce, hasNonce := params["nonce"]
	if !hasRealm || !hasNonce || nonce == "" {
		return "", errors.New("digest challenge incomplete")
	}
	// realm/nonce/opaque are foreign bytes that get echoed into the
	// Authorization header; a control character in them would be a
	// header-injection vector, so an unclean challenge is refused.
	if hasCTL(realm) || hasCTL(nonce) || hasCTL(params["opaque"]) {
		return "", errors.New("digest challenge carries control characters")
	}

	if !strings.EqualFold(params["algorithm"], "SHA-256") {
		return "", errors.New("unsupported digest algorithm (SHA-256 only)")
	}
	const algorithm = "SHA-256"
	h := func(parts ...string) string {
		sum := sha256.Sum256([]byte(strings.Join(parts, ":")))
		return hex.EncodeToString(sum[:])
	}

	// qop may list several tokens ("auth,auth-int"); we can only do
	// plain auth. A challenge offering ONLY auth-int is out of scope.
	qop := ""
	for _, tok := range strings.Split(params["qop"], ",") {
		if strings.EqualFold(strings.TrimSpace(tok), "auth") {
			qop = "auth"
			break
		}
	}
	if params["qop"] != "" && qop == "" {
		return "", errors.New("unsupported digest qop")
	}

	ha1 := h(username, realm, password)
	ha2 := h(method, uri)

	var response string
	const nc = "00000001"
	cnonce := ""
	if qop == "auth" {
		var err error
		if cnonce, err = randomCnonce(); err != nil {
			return "", err
		}
		response = h(ha1, nonce, nc, cnonce, qop, ha2)
	} else {
		response = h(ha1, nonce, ha2)
	}

	var b strings.Builder
	b.WriteString("Digest ")
	writeParam := func(key, val string, quote bool) {
		if b.Len() > len("Digest ") {
			b.WriteString(", ")
		}
		b.WriteString(key)
		b.WriteByte('=')
		if quote {
			b.WriteByte('"')
			b.WriteString(quoteEscape(val))
			b.WriteByte('"')
		} else {
			b.WriteString(val)
		}
	}
	writeParam("username", username, true)
	writeParam("realm", realm, true)
	writeParam("nonce", nonce, true)
	writeParam("uri", uri, true)
	writeParam("response", response, true)
	writeParam("algorithm", algorithm, false)
	if qop == "auth" {
		writeParam("qop", qop, false)
		writeParam("nc", nc, false)
		writeParam("cnonce", cnonce, true)
	}
	if opaque, ok := params["opaque"]; ok {
		writeParam("opaque", opaque, true)
	}
	return b.String(), nil
}

// randomCnonce returns 16 random bytes as hex for the client nonce.
// A package variable so the digest test can pin the RFC 7616 example
// cnonce and assert the exact response hash.
var randomCnonce = defaultCnonce

func defaultCnonce() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", errors.New("cnonce: no randomness available")
	}
	return hex.EncodeToString(buf[:]), nil
}

// splitChallenge separates the auth scheme from its comma-separated
// key=value parameters, unquoting quoted-string values (including
// backslash escapes). Parameter names are lowercased.
func splitChallenge(header string) (scheme string, params map[string]string) {
	header = strings.TrimSpace(header)
	params = map[string]string{}
	sp := strings.IndexAny(header, " \t")
	if sp < 0 {
		return header, params
	}
	scheme = header[:sp]
	rest := header[sp+1:]

	// Walk the parameter list by hand: values may be quoted strings
	// containing commas, so a plain strings.Split would tear them.
	for i := 0; i < len(rest); {
		// skip separators
		for i < len(rest) && (rest[i] == ',' || rest[i] == ' ' || rest[i] == '\t') {
			i++
		}
		if i >= len(rest) {
			break
		}
		// key
		start := i
		for i < len(rest) && rest[i] != '=' && rest[i] != ',' {
			i++
		}
		if i >= len(rest) || rest[i] != '=' {
			continue // malformed token without a value - skip it
		}
		key := strings.ToLower(strings.TrimSpace(rest[start:i]))
		i++ // consume '='
		// value: quoted-string or token
		var val string
		if i < len(rest) && rest[i] == '"' {
			i++
			var sb strings.Builder
			for i < len(rest) && rest[i] != '"' {
				if rest[i] == '\\' && i+1 < len(rest) {
					i++
				}
				sb.WriteByte(rest[i])
				i++
			}
			i++ // consume closing quote (or run off the end - tolerated)
			val = sb.String()
		} else {
			start = i
			for i < len(rest) && rest[i] != ',' {
				i++
			}
			val = strings.TrimSpace(rest[start:i])
		}
		if key != "" {
			params[key] = val
		}
	}
	return scheme, params
}

// quoteEscape escapes backslashes and double quotes for a
// quoted-string parameter value.
func quoteEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

// hasCTL reports whether s contains an ASCII control character
// (including CR/LF - the header-injection bytes).
func hasCTL(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}
