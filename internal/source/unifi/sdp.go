package unifi

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
)

// sdpSecurityReport scans the raw SDP body for security-relevant attributes
// and returns a single-line, log-safe summary.
//
// Logged information:
//   - all `m=` lines (so the RTP profile is visible: RTP/AVP vs RTP/SAVP)
//   - count and summary of `a=crypto:` (SDES, RFC 4568) — suite name and
//     decoded inline-key length, NEVER the key bytes themselves
//   - count and summary of `a=key-mgmt:` (MIKEY, RFC 3830) — method only
//   - the sorted set of other `a=` attribute names seen (values omitted)
//
// gortsplib v5 only handles `a=key-mgmt:mikey ...` and the RTP profile
// flag in the `m=` line to decide whether to do SRTP. It silently drops
// any `a=crypto:` (SDES) attribute. This summary lets us see exactly
// what the camera advertises, without leaking the inline key into the
// log file.
//
// The body should be the SDP exactly as the server returned it (i.e.
// base.Response.Body from a Describe call).
func sdpSecurityReport(body []byte) string {
	var mLines []string
	var cryptos []string
	var keyMgmts []string
	otherAttrs := map[string]struct{}{}

	for _, raw := range bytes.Split(body, []byte("\n")) {
		line := strings.TrimRight(string(raw), "\r ")
		if len(line) < 2 || line[1] != '=' {
			continue
		}
		prefix, rest := line[0], line[2:]

		switch prefix {
		case 'm':
			mLines = append(mLines, rest)
		case 'a':
			name := rest
			value := ""
			if idx := strings.Index(rest, ":"); idx >= 0 {
				name = rest[:idx]
				value = rest[idx+1:]
			}
			switch name {
			case "crypto":
				cryptos = append(cryptos, summarizeCryptoValue(value))
			case "key-mgmt":
				keyMgmts = append(keyMgmts, summarizeKeyMgmtValue(value))
			default:
				otherAttrs[name] = struct{}{}
			}
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "m-lines=%d", len(mLines))
	for _, m := range mLines {
		fmt.Fprintf(&b, " [%s]", m)
	}
	fmt.Fprintf(&b, "; a=crypto count=%d", len(cryptos))
	for _, c := range cryptos {
		fmt.Fprintf(&b, " {%s}", c)
	}
	fmt.Fprintf(&b, "; a=key-mgmt count=%d", len(keyMgmts))
	for _, k := range keyMgmts {
		fmt.Fprintf(&b, " {%s}", k)
	}

	names := make([]string, 0, len(otherAttrs))
	for n := range otherAttrs {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Fprintf(&b, "; other a= attrs=%v", names)
	return b.String()
}

// summarizeCryptoValue redacts the secret payload of an `a=crypto:` value
// while preserving the diagnostic shape (suite, key-method, decoded key
// length, lifetime / MKI present).
//
// Input shape (RFC 4568 §9.2):
//
//	<tag> <crypto-suite> <key-params>+ [<session-params>+]
//
// where a typical key-params is `inline:<base64key>|<lifetime>|<mki:length>`.
func summarizeCryptoValue(value string) string {
	fields := strings.Fields(value)
	if len(fields) < 2 {
		return "malformed (too few fields)"
	}
	tag, suite := fields[0], fields[1]
	summary := fmt.Sprintf("tag=%s suite=%s", tag, suite)

	if len(fields) >= 3 {
		// Only the first key-params; subsequent ones get the same treatment.
		for _, kp := range fields[2:] {
			method, val, ok := strings.Cut(kp, ":")
			if !ok {
				summary += " " + method + "=<malformed>"
				continue
			}
			switch method {
			case "inline":
				// <base64key>|<lifetime>|<mki:length>
				keyPart, rest, _ := strings.Cut(val, "|")
				keyLen := 0
				if dec, err := base64.StdEncoding.DecodeString(keyPart); err == nil {
					keyLen = len(dec)
				}
				summary += fmt.Sprintf(" inline=<redacted %dB>", keyLen)
				if rest != "" {
					// rest already has the leading "|" stripped by Cut;
					// log the non-secret meta fields verbatim.
					summary += " params=" + rest
				}
			default:
				// Unknown key-method (e.g. "uri:"). Redact the value to be safe.
				summary += " " + method + "=<redacted>"
			}
		}
	}
	return summary
}

// summarizeKeyMgmtValue reports the method (e.g. "mikey") and the
// presence + length of any base64 blob that follows, without the blob
// itself.
//
//	a=key-mgmt:mikey AQAFAQABAAAAAAA...   (RFC 4567 + 3830)
func summarizeKeyMgmtValue(value string) string {
	method, payload, ok := strings.Cut(value, " ")
	if !ok {
		return "method=" + value
	}
	if dec, err := base64.StdEncoding.DecodeString(payload); err == nil {
		return fmt.Sprintf("method=%s payload=<redacted %dB>", method, len(dec))
	}
	return fmt.Sprintf("method=%s payload=<%d chars, not base64>", method, len(payload))
}
