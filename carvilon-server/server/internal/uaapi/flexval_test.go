package uaapi

import (
	"encoding/json"
	"testing"
)

func TestFlexVal_StringAcrossTypes(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{`"open"`, "open"},
		{`123`, "123"},
		{`true`, "true"},
		{`false`, "false"},
		{`null`, ""},
		{`""`, ""},
		{`"  spaced  "`, "  spaced  "},
	}
	for _, c := range cases {
		var f flexVal
		if err := json.Unmarshal([]byte(c.raw), &f); err != nil {
			t.Fatalf("unmarshal %s: %v", c.raw, err)
		}
		if got := f.String(); got != c.want {
			t.Errorf("String(%s) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestFlexVal_Empty(t *testing.T) {
	for _, raw := range []string{`null`, `""`, `   `} {
		var f flexVal
		_ = json.Unmarshal([]byte(raw), &f)
		if !f.Empty() {
			t.Errorf("Empty(%s) = false, want true", raw)
		}
	}
	var zero flexVal
	if !zero.Empty() {
		t.Errorf("zero flexVal not Empty")
	}
	var f flexVal
	_ = json.Unmarshal([]byte(`"x"`), &f)
	if f.Empty() {
		t.Errorf("Empty for non-empty string = true")
	}
}

func TestFlexVal_Bool(t *testing.T) {
	cases := []struct {
		raw     string
		wantVal bool
		wantOK  bool
	}{
		{`true`, true, true},
		{`false`, false, true},
		{`1`, true, true},
		{`0`, false, true},
		{`"true"`, true, true},
		{`"yes"`, true, true},
		{`"no"`, false, true},
		{`"maybe"`, false, false},
		{`42`, false, false},
		{`null`, false, false},
	}
	for _, c := range cases {
		var f flexVal
		_ = json.Unmarshal([]byte(c.raw), &f)
		v, ok := f.Bool()
		if v != c.wantVal || ok != c.wantOK {
			t.Errorf("Bool(%s) = (%v,%v), want (%v,%v)", c.raw, v, ok, c.wantVal, c.wantOK)
		}
	}
}

// A struct field of type flexVal never fails a list decode, even when a
// firmware sends an unexpected scalar type for it.
func TestFlexVal_DecodeNeverBreaksList(t *testing.T) {
	type row struct {
		Status flexVal `json:"status"`
	}
	// Same field arrives as string, int and bool across records.
	raw := `[{"status":"open"},{"status":3},{"status":true}]`
	var rows []row
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		t.Fatalf("decode mixed-type list: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("len = %d, want 3", len(rows))
	}
	if rows[0].Status.String() != "open" || rows[1].Status.String() != "3" || rows[2].Status.String() != "true" {
		t.Errorf("mixed decode = %q/%q/%q", rows[0].Status.String(), rows[1].Status.String(), rows[2].Status.String())
	}
}
