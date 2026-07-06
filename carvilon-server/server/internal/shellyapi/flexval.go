// flexVal is a JSON value that never fails to unmarshal - the same
// tolerant-decode helper internal/uaapi and internal/protectapi use
// (Saison 21). The Shelly Gen2+ RPC API is a foreign surface whose
// field types may drift across firmware revisions; typing an uncertain
// field as `float64` or `*bool` would make ONE surprising value fail a
// whole GetStatus decode and blank the Switches category. flexVal
// keeps the raw bytes and interprets them lazily on read, so the
// payload always decodes and the field just renders as whatever the
// device sent.
package shellyapi

import (
	"bytes"
	"encoding/json"
	"strings"
)

// flexVal holds a single JSON value (scalar, object or array) as its
// raw bytes. Its zero value is an absent value (Empty() == true).
type flexVal struct {
	raw json.RawMessage
}

// UnmarshalJSON stores the raw bytes verbatim and never errors.
func (f *flexVal) UnmarshalJSON(b []byte) error {
	f.raw = append(f.raw[:0], b...)
	return nil
}

// MarshalJSON round-trips the captured value (null when absent).
func (f flexVal) MarshalJSON() ([]byte, error) {
	if len(bytes.TrimSpace(f.raw)) == 0 {
		return []byte("null"), nil
	}
	return f.raw, nil
}

// Empty reports whether the value is absent, JSON null or an empty
// string - the cases the Device Center renders as "-".
func (f flexVal) Empty() bool {
	s := string(bytes.TrimSpace(f.raw))
	return s == "" || s == "null" || s == `""`
}

// String renders the value for display: JSON strings are unquoted,
// numbers and bools are kept verbatim, null/absent become "". Objects
// and arrays fall back to their compact raw JSON.
func (f flexVal) String() string {
	s := bytes.TrimSpace(f.raw)
	if len(s) == 0 || string(s) == "null" {
		return ""
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(s, &str); err == nil {
			return str
		}
	}
	return string(s)
}

// Bool interprets the value as a boolean. ok is false when the value
// is not a recognisable boolean (true/false, 1/0, yes/no), so callers
// can distinguish "false" from "not a bool".
func (f flexVal) Bool() (val, ok bool) {
	switch strings.ToLower(strings.TrimSpace(f.String())) {
	case "true", "1", "yes":
		return true, true
	case "false", "0", "no":
		return false, true
	default:
		return false, false
	}
}
