package designer

import (
	"testing"

	"carvilon.local/server/internal/engine"
)

// TestCatalog_CountsAndCategories pins the palette shape the editor
// renders: 111 blocks across exactly the five categories, in the
// counts the former inline list had.
func TestCatalog_CountsAndCategories(t *testing.T) {
	blocks := Catalog()
	if len(blocks) != 111 {
		t.Fatalf("Catalog() has %d blocks, want 111", len(blocks))
	}
	want := map[string]int{"input": 26, "logic": 26, "time": 22, "memory": 13, "output": 24}
	got := map[string]int{}
	for _, b := range blocks {
		got[b.Category]++
	}
	if len(got) != len(want) {
		t.Errorf("categories = %v, want keys %v", got, want)
	}
	for cat, n := range want {
		if got[cat] != n {
			t.Errorf("category %q has %d blocks, want %d", cat, got[cat], n)
		}
	}
}

// TestCatalog_Shape asserts every block is well-formed: non-empty
// identity fields, unique types, never-nil port/param slices (so the
// JSON carries [] not null), and only valid kinds.
func TestCatalog_Shape(t *testing.T) {
	validKind := map[string]bool{"bool": true, "float": true, "text": true}
	seen := map[string]bool{}
	for _, b := range Catalog() {
		if b.Type == "" || b.Category == "" || b.Title == "" || b.Icon == "" {
			t.Errorf("block %+v has an empty identity field", b)
		}
		if seen[b.Type] {
			t.Errorf("duplicate type %q", b.Type)
		}
		seen[b.Type] = true
		if b.Inputs == nil || b.Outputs == nil || b.Params == nil {
			t.Errorf("block %q has a nil port/param slice (want empty)", b.Type)
		}
		for _, p := range append(append([]CatalogPort{}, b.Inputs...), b.Outputs...) {
			if p.Name == "" || !validKind[p.Kind] {
				t.Errorf("block %q port %+v invalid", b.Type, p)
			}
		}
		for _, p := range b.Params {
			if p.Name == "" || !validKind[p.Kind] {
				t.Errorf("block %q param %+v invalid", b.Type, p)
			}
		}
	}
}

// TestCatalog_Implemented checks that exactly the four engine-backed
// blocks are flagged implemented and that their ports/params/delay-
// boundary are derived faithfully from the engine registry (the single
// source of truth), while every other block stays catalog-only.
func TestCatalog_Implemented(t *testing.T) {
	wantImpl := map[string]bool{
		"input.manual":   true,
		"logic.or":       true,
		"time.staircase": true,
		"output.lamp":    true,
	}
	implCount := 0
	for _, b := range Catalog() {
		if !b.Implemented {
			if len(b.Inputs) != 0 || len(b.Outputs) != 0 || len(b.Params) != 0 {
				t.Errorf("catalog-only block %q unexpectedly carries ports/params", b.Type)
			}
			continue
		}
		implCount++
		if !wantImpl[b.Type] {
			t.Errorf("unexpected implemented type %q", b.Type)
			continue
		}
		d, ok := engine.Lookup(b.Type)
		if !ok {
			t.Errorf("implemented type %q not in engine registry", b.Type)
			continue
		}
		if b.DelayBoundary != d.DelayBoundary {
			t.Errorf("%q delay_boundary = %v, want %v", b.Type, b.DelayBoundary, d.DelayBoundary)
		}
		assertPorts(t, b.Type, "inputs", b.Inputs, d.Inputs)
		assertPorts(t, b.Type, "outputs", b.Outputs, d.Outputs)
		if len(b.Params) != len(d.Params) {
			t.Errorf("%q has %d params, want %d", b.Type, len(b.Params), len(d.Params))
		} else {
			for i, p := range d.Params {
				if b.Params[i].Name != p.Name || b.Params[i].Kind != kindString(p.Kind) {
					t.Errorf("%q param %d = %+v, want name=%q kind=%q", b.Type, i, b.Params[i], p.Name, kindString(p.Kind))
				}
			}
		}
	}
	if implCount != len(wantImpl) {
		t.Errorf("implemented count = %d, want %d", implCount, len(wantImpl))
	}
}

func assertPorts(t *testing.T, typ, side string, got []CatalogPort, want []engine.Port) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%q %s has %d ports, want %d", typ, side, len(got), len(want))
		return
	}
	for i, p := range want {
		if got[i].Name != p.Name || got[i].Kind != kindString(p.Kind) {
			t.Errorf("%q %s[%d] = %+v, want name=%q kind=%q", typ, side, i, got[i], p.Name, kindString(p.Kind))
		}
	}
}
