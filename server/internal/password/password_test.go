package password

import (
	"strings"
	"testing"
)

func TestGenerate_Length(t *testing.T) {
	for i := 0; i < 50; i++ {
		p, err := Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if len(p) != 16 {
			t.Errorf("len = %d, want 16: %s", len(p), p)
		}
	}
}

func TestGenerate_Format(t *testing.T) {
	p, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if p[4] != '-' || p[10] != '-' {
		t.Errorf("dash positions wrong in %s", p)
	}
}

func TestGenerate_HasMixedCharacterClasses(t *testing.T) {
	for i := 0; i < 100; i++ {
		p, _ := Generate()
		stripped := strings.ReplaceAll(p, "-", "")
		if !hasUpper(stripped) {
			t.Errorf("missing uppercase: %s", p)
		}
		if !hasLower(stripped) {
			t.Errorf("missing lowercase: %s", p)
		}
		if !hasDigit(stripped) {
			t.Errorf("missing digit: %s", p)
		}
	}
}

func TestGenerate_NotPredictable(t *testing.T) {
	a, _ := Generate()
	b, _ := Generate()
	if a == b {
		t.Errorf("two consecutive generations identical: %s", a)
	}
}
