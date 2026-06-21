package featuregate

import (
	"testing"

	"carvilon.local/server/internal/viewermanager"
)

func ptrBool(b bool) *bool { return &b }
func strPtr(s string) *string { return &s }

// feat looks a catalog feature up by key for the precedence tests.
func feat(key string) Feature {
	for _, f := range DefaultCatalog() {
		if f.Key == key {
			return f
		}
	}
	panic("unknown feature " + key)
}

// keep_stream type default (no viewer value, no template, no license rows):
// ESP closes the stream (false), the Android app / web stay connected (true).
// This is the proven NULL-inherits testcase, reused (not rebuilt) via the
// catalog bridge delegating to ResolveKeepStream*().
func TestResolve_KeepStreamTypeDefault(t *testing.T) {
	f := feat(KeyKeepStreamInScreensaver)

	esp := &viewermanager.ViewerInfo{Type: viewermanager.TypeESP}
	if got := Resolve(f, Snapshot{}, esp); got.Value != false || !got.Licensed || !got.Active {
		t.Errorf("ESP default: got %+v, want {Licensed:true Active:true Value:false}", got)
	}
	android := &viewermanager.ViewerInfo{Type: viewermanager.TypeAndroid}
	if got := Resolve(f, Snapshot{}, android); got.Value != true {
		t.Errorf("Android default: Value = %v, want true", got.Value)
	}
}

// value precedence: viewer column (set) > template value > catalog/type default.
func TestResolve_ValuePrecedence(t *testing.T) {
	f := feat(KeyKeepStreamInScreensaver)
	esp := func(set *bool) *viewermanager.ViewerInfo {
		return &viewermanager.ViewerInfo{Type: viewermanager.TypeESP, KeepStreamInScreensaver: set}
	}
	tmplFalse := &Template{value: map[string]string{KeyKeepStreamInScreensaver: "false"}}
	tmplTrue := &Template{value: map[string]string{KeyKeepStreamInScreensaver: "true"}}

	// (a) viewer column set (explicit true) wins over template "false" + ESP default false.
	if got := Resolve(f, Snapshot{Template: tmplFalse}, esp(ptrBool(true))); got.Value != true {
		t.Errorf("viewer-wins: Value = %v, want true", got.Value)
	}
	// (b) viewer unset -> template "true" wins over ESP default false.
	if got := Resolve(f, Snapshot{Template: tmplTrue}, esp(nil)); got.Value != true {
		t.Errorf("template-wins: Value = %v, want true", got.Value)
	}
	// (c) viewer unset, no template -> ESP type default false.
	if got := Resolve(f, Snapshot{}, esp(nil)); got.Value != false {
		t.Errorf("default: Value = %v, want false", got.Value)
	}
}

// active precedence: viewer override > template active > catalog DefaultActive.
func TestResolve_ActivePrecedence(t *testing.T) {
	f := feat(KeyKeepStreamInScreensaver) // DefaultActive true
	info := &viewermanager.ViewerInfo{Type: viewermanager.TypeAndroid}

	if got := Resolve(f, Snapshot{}, info); !got.Active {
		t.Errorf("default active: got false, want true")
	}
	tmplOff := &Template{active: map[string]bool{KeyKeepStreamInScreensaver: false}}
	if got := Resolve(f, Snapshot{Template: tmplOff}, info); got.Active {
		t.Errorf("template active=false: got true, want false")
	}
	snap := Snapshot{
		Template:  tmplOff,
		Overrides: map[string]bool{KeyKeepStreamInScreensaver: true},
	}
	if got := Resolve(f, snap, info); !got.Active {
		t.Errorf("viewer override active=true: got false, want true (wins over template)")
	}
}

// license gate: not licensed -> locked (Licensed/Active false, Value nil),
// regardless of any viewer/template setting.
func TestResolve_LicenseGateLocks(t *testing.T) {
	f := feat(KeyKeepStreamInScreensaver)
	info := &viewermanager.ViewerInfo{Type: viewermanager.TypeAndroid, KeepStreamInScreensaver: ptrBool(true)}
	snap := Snapshot{
		License:  License{features: map[string]bool{KeyKeepStreamInScreensaver: false}},
		Template: &Template{value: map[string]string{KeyKeepStreamInScreensaver: "true"}},
	}
	got := Resolve(f, snap, info)
	if got.Licensed || got.Active || got.Value != nil {
		t.Errorf("locked: got %+v, want {Licensed:false Active:false Value:<nil>}", got)
	}
}

// license absence = catalog DefaultLicensed (today's settings stay licensed).
func TestResolve_LicenseAbsenceIsCatalogDefault(t *testing.T) {
	f := feat(KeyKeepStreamInScreensaver)
	info := &viewermanager.ViewerInfo{Type: viewermanager.TypeAndroid}
	if got := Resolve(f, Snapshot{}, info); !got.Licensed {
		t.Errorf("absence: Licensed = false, want true (catalog default)")
	}
}

// a malformed template value never breaks delivery: it falls back to the
// catalog/type default.
func TestResolve_ParseErrorFallsBackToDefault(t *testing.T) {
	f := feat(KeyKeepStreamInScreensaver)
	info := &viewermanager.ViewerInfo{Type: viewermanager.TypeAndroid} // default true
	snap := Snapshot{Template: &Template{value: map[string]string{KeyKeepStreamInScreensaver: "notabool"}}}
	got := Resolve(f, snap, info)
	if !got.Licensed || got.Value != true {
		t.Errorf("parse-error fallback: got %+v, want Licensed:true Value:true (default)", got)
	}
}

// the generic TEXT->type parse path works for a non-bool feature, and its parse
// error also falls back to the default (exercises parseInt through Resolve).
func TestResolve_GenericIntTemplateValue(t *testing.T) {
	f := Feature{
		Key: "synthetic_int", Type: TypeInt, DefaultActive: true, DefaultLicensed: true,
		ViewerValueSet: func(*viewermanager.ViewerInfo) bool { return false },
		ResolveValue:   func(*viewermanager.ViewerInfo) any { return 5 },
		ParseValue:     parseInt,
	}
	snap := Snapshot{Template: &Template{value: map[string]string{"synthetic_int": "42"}}}
	if got := Resolve(f, snap, nil); got.Value != 42 {
		t.Errorf("int template value: got %v, want 42", got.Value)
	}
	bad := Snapshot{Template: &Template{value: map[string]string{"synthetic_int": "NaN"}}}
	if got := Resolve(f, bad, nil); got.Value != 5 {
		t.Errorf("int parse error: got %v, want 5 (default)", got.Value)
	}
}

func TestParseHelpers(t *testing.T) {
	if v, err := parseBool("true"); err != nil || v != true {
		t.Errorf("parseBool: %v %v", v, err)
	}
	if v, err := parseInt("7"); err != nil || v != 7 {
		t.Errorf("parseInt: %v %v", v, err)
	}
	if _, err := parseInt("x"); err == nil {
		t.Errorf("parseInt(x): want error")
	}
	if v, err := parseString("hi"); err != nil || v != "hi" {
		t.Errorf("parseString: %v %v", v, err)
	}
	p := parseEnum([]string{"a", "b"})
	if v, err := p("a"); err != nil || v != "a" {
		t.Errorf("parseEnum(a): %v %v", v, err)
	}
	if _, err := p("z"); err == nil {
		t.Errorf("parseEnum(z): want error")
	}
}

func TestEffectiveBool(t *testing.T) {
	if !(Effective{Value: true}).Bool(false) {
		t.Error("Bool(true) -> false")
	}
	if !(Effective{Value: nil}).Bool(true) {
		t.Error("Bool(nil) should return default")
	}
	if !(Effective{Value: 5}).Bool(true) {
		t.Error("Bool(non-bool) should return default")
	}
}
