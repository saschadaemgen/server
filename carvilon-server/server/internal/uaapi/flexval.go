// Saison 21 (UA read-only overview): flexVal is a JSON value that
// never fails to unmarshal. The live UDM's device/door records carry
// a handful of status fields (door_position_status, is_bind_hub, ...)
// whose exact type is not pinned down across firmware revisions - a
// value we expect as a string could arrive as an int enum or a bool.
// Typing such a field as `string` or `*bool` would make ONE surprising
// value fail the whole /devices or /doors list decode and blank the
// overview. flexVal keeps the raw bytes and interprets them lazily on
// read, so the list always decodes and the field just renders as
// whatever the UDM sent.
package uaapi

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
// string - the cases the overview renders as "unbekannt"/"—".
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
