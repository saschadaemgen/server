package uaapi

import (
	"encoding/json"
	"strings"
	"testing"
)

type miniThing struct {
	ID string `json:"id"`
}

func TestDecodeList_Empty(t *testing.T) {
	for _, raw := range []string{"", "null"} {
		got, err := decodeList[miniThing]([]byte(raw))
		if err != nil {
			t.Errorf("%q: err = %v", raw, err)
		}
		if len(got) != 0 {
			t.Errorf("%q: len = %d", raw, len(got))
		}
	}
}

func TestDecodeList_FlatArray(t *testing.T) {
	got, err := decodeList[miniThing](json.RawMessage(`[{"id":"a"},{"id":"b"}]`))
	if err != nil {
		t.Fatalf("flat: %v", err)
	}
	if len(got) != 2 || got[1].ID != "b" {
		t.Errorf("flat = %+v", got)
	}
}

func TestDecodeList_ArrayOfArrays(t *testing.T) {
	got, err := decodeList[miniThing](json.RawMessage(`[[{"id":"a"}],[{"id":"b"},{"id":"c"}]]`))
	if err != nil {
		t.Fatalf("nested: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].ID != "a" || got[1].ID != "b" || got[2].ID != "c" {
		t.Errorf("flatten order wrong: %+v", got)
	}
}

func TestDecodeList_WrapperVariants(t *testing.T) {
	cases := map[string]string{
		"list":    `{"list":[{"id":"a"}]}`,
		"devices": `{"devices":[{"id":"b"}]}`,
		"doors":   `{"doors":[{"id":"c"}]}`,
		"items":   `{"items":[{"id":"d"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := decodeList[miniThing](json.RawMessage(body))
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("len = %d", len(got))
			}
		})
	}
}

func TestDecodeList_UnknownShapeError(t *testing.T) {
	_, err := decodeList[miniThing](json.RawMessage(`123`))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "unknown list response shape") {
		t.Errorf("err = %v, want unknown-shape", err)
	}
	if !strings.Contains(err.Error(), "123") {
		t.Errorf("err = %v, want payload included", err)
	}
}

func TestTruncateForError(t *testing.T) {
	long := strings.Repeat("x", 300)
	got := truncateForError([]byte(long), 100)
	if len(got) != 103 || !strings.HasSuffix(got, "...") {
		t.Errorf("truncate = %q (len %d)", got, len(got))
	}
	short := truncateForError([]byte("ok"), 100)
	if short != "ok" {
		t.Errorf("short = %q", short)
	}
}
