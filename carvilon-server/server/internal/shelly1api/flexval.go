// flexVal is a JSON value that never fails to unmarshal - the same
// tolerant-decode helper internal/shellyapi, internal/uaapi and
// internal/protectapi use. The Gen1 REST API is a frozen but foreign
// surface whose field types vary across the fleet's firmware spread;
// typing an uncertain field as `float64` or `*bool` would make ONE
// surprising value fail a whole /status decode and blank the device row.
// flexVal keeps the raw bytes and interprets them lazily on read, so the
// payload always decodes and the field just renders as whatever the
// device sent. (Copied, not imported: shellyapi keeps it unexported and
// the two clients stay independent by design.)
package shelly1api

import (
	"bytes"
	"encoding/json"
	"strconv"
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

// Float interprets the value as a number. ok is false when the value is
// absent or not numeric (Gen1 meters report bare numbers; "-" renders
// for anything else).
func (f flexVal) Float() (val float64, ok bool) {
	s := strings.TrimSpace(f.String())
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
