package featuregate

import (
	"testing"

	"carvilon.local/server/internal/viewermanager"
)

func ptrBool(b bool) *bool     { return &b }
func strPtr(s string) *string  { return &s }

// feat looks a catalog feature up by key for the precedence tests.
func feat(key string) Feature {
	f, ok := Lookup(key)
	if !ok {
		panic("unknown feature " + key)
	}
	return f
}

// keep_stream type default (no viewer value, no template, no license rows):
// ESP closes the stream (false), the Android app / web stay connected (true).
// Default exposure is tenant_visible.
func TestResolve_KeepStreamTypeDefault(t *testing.T) {
	f := feat(KeyKeepStreamInScreensaver)

	esp := &viewermanager.ViewerInfo{Type: viewermanager.TypeESP}
	got := Resolve(f, Snapshot{}, esp)
	if got.Value != false || !got.Licensed || got.Exposure != ExposureTenantVisible || !got.Writable {
		t.Errorf("ESP default: got %+v, want {Licensed:true Exposure:tenant_visible Value:false Writable:true}", got)
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

	if got := Resolve(f, Snapshot{Template: tmplFalse}, esp(ptrBool(true))); got.Value != true {
		t.Errorf("viewer-wins: Value = %v, want true", got.Value)
	}
	if got := Resolve(f, Snapshot{Template: tmplTrue}, esp(nil)); got.Value != true {
		t.Errorf("template-wins: Value = %v, want true", got.Value)
	}
	if got := Resolve(f, Snapshot{}, esp(nil)); got.Value != false {
		t.Errorf("default: Value = %v, want false", got.Value)
	}
}

// exposure=hidden forces the catalog/type default and IGNORES the viewer value.
func TestResolve_HiddenForcesDefault(t *testing.T) {
	f := feat(KeyKeepStreamInScreensaver)
	// ESP viewer with an EXPLICIT true; default for ESP is false.
	esp := &viewermanager.ViewerInfo{Type: viewermanager.TypeESP, KeepStreamInScreensaver: ptrBool(true)}
	snap := Snapshot{Overrides: map[string]string{KeyKeepStreamInScreensaver: ExposureHidden}}
	got := Resolve(f, snap, esp)
	if got.Value != false {
		t.Errorf("hidden: Value = %v, want false (forced ESP default, override ignored)", got.Value)
	}
	if got.Exposure != ExposureHidden || got.Writable || got.TenantVisible() {
		t.Errorf("hidden: got %+v, want exposure hidden, not writable, not tenant-visible", got)
	}
}

// exposure=admin_only keeps the admin-set value (it applies); not tenant-writable.
func TestResolve_AdminOnlyValueApplies(t *testing.T) {
	f := feat(KeyKeepStreamInScreensaver)
	esp := &viewermanager.ViewerInfo{Type: viewermanager.TypeESP, KeepStreamInScreensaver: ptrBool(true)}
	snap := Snapshot{Overrides: map[string]string{KeyKeepStreamInScreensaver: ExposureAdminOnly}}
	got := Resolve(f, snap, esp)
	if got.Value != true {
		t.Errorf("admin_only: Value = %v, want true (admin value applies)", got.Value)
	}
	if got.Writable || got.TenantVisible() {
		t.Errorf("admin_only: got writable/tenantVisible true, want false")
	}
}

// exposure precedence: viewer override > template > default(tenant_visible).
func TestResolve_ExposurePrecedence(t *testing.T) {
	f := feat(KeyKeepStreamInScreensaver)
	info := &viewermanager.ViewerInfo{Type: viewermanager.TypeAndroid}

	if got := Resolve(f, Snapshot{}, info); got.Exposure != ExposureTenantVisible {
		t.Errorf("default exposure = %q, want tenant_visible", got.Exposure)
	}
	tmplAdmin := &Template{exposure: map[string]string{KeyKeepStreamInScreensaver: ExposureAdminOnly}}
	if got := Resolve(f, Snapshot{Template: tmplAdmin}, info); got.Exposure != ExposureAdminOnly {
		t.Errorf("template exposure = %q, want admin_only", got.Exposure)
	}
	snap := Snapshot{
		Template:  tmplAdmin,
		Overrides: map[string]string{KeyKeepStreamInScreensaver: ExposureHidden},
	}
	if got := Resolve(f, snap, info); got.Exposure != ExposureHidden {
		t.Errorf("viewer override exposure = %q, want hidden (wins over template)", got.Exposure)
	}
}

// writable = licensed && tenant_visible && has write bridge.
func TestResolve_WritableDerivation(t *testing.T) {
	info := &viewermanager.ViewerInfo{Type: viewermanager.TypeAndroid}
	ks := feat(KeyKeepStreamInScreensaver) // write-capable

	if got := Resolve(ks, Snapshot{}, info); !got.Writable {
		t.Errorf("tenant_visible + licensed + bridge: want writable")
	}
	adminSnap := Snapshot{Overrides: map[string]string{KeyKeepStreamInScreensaver: ExposureAdminOnly}}
	if got := Resolve(ks, adminSnap, info); got.Writable {
		t.Errorf("admin_only: want not writable")
	}
	// Legacy key is tenant_visible by default but has NO write bridge -> never writable.
	lang := feat(KeyLanguage)
	if got := Resolve(lang, Snapshot{}, info); got.Writable {
		t.Errorf("legacy key without write bridge: want not writable")
	}
	if got := Resolve(lang, Snapshot{}, info); got.Exposure != ExposureTenantVisible {
		t.Errorf("legacy key exposure = %q, want tenant_visible (default)", got.Exposure)
	}
}

// license gate: not licensed -> locked (Licensed/Writable false, Value nil).
func TestResolve_LicenseGateLocks(t *testing.T) {
	f := feat(KeyKeepStreamInScreensaver)
	info := &viewermanager.ViewerInfo{Type: viewermanager.TypeAndroid, KeepStreamInScreensaver: ptrBool(true)}
	snap := Snapshot{License: License{features: map[string]bool{KeyKeepStreamInScreensaver: false}}}
	got := Resolve(f, snap, info)
	if got.Licensed || got.Writable || got.Value != nil {
		t.Errorf("locked: got %+v, want {Licensed:false Writable:false Value:<nil>}", got)
	}
}

func TestResolve_LicenseAbsenceIsCatalogDefault(t *testing.T) {
	f := feat(KeyKeepStreamInScreensaver)
	info := &viewermanager.ViewerInfo{Type: viewermanager.TypeAndroid}
	if got := Resolve(f, Snapshot{}, info); !got.Licensed {
		t.Errorf("absence: Licensed = false, want true (catalog default)")
	}
}

// a malformed template value falls back to the catalog/type default.
func TestResolve_ParseErrorFallsBackToDefault(t *testing.T) {
	f := feat(KeyKeepStreamInScreensaver)
	info := &viewermanager.ViewerInfo{Type: viewermanager.TypeAndroid} // default true
	snap := Snapshot{Template: &Template{value: map[string]string{KeyKeepStreamInScreensaver: "notabool"}}}
	if got := Resolve(f, snap, info); !got.Licensed || got.Value != true {
		t.Errorf("parse-error fallback: got %+v, want Licensed:true Value:true", got)
	}
}

func TestResolve_GenericIntTemplateValue(t *testing.T) {
	f := Feature{
		Key: "synthetic_int", Type: TypeInt, DefaultLicensed: true,
		ViewerValueSet: func(*viewermanager.ViewerInfo) bool { return false },
		DefaultValue:   func(*viewermanager.ViewerInfo) any { return 5 },
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

func TestValidExposure(t *testing.T) {
	// Saison 20: bookable is now a valid state (resolves like hidden).
	for _, e := range []string{ExposureTenantVisible, ExposureAdminOnly, ExposureHidden, ExposureBookable} {
		if !ValidExposure(e) {
			t.Errorf("ValidExposure(%q) = false, want true", e)
		}
	}
	if ValidExposure("nonsense") {
		t.Errorf("ValidExposure(nonsense) = true, want false")
	}
}

func TestCoerceWriteValue(t *testing.T) {
	ks := feat(KeyKeepStreamInScreensaver)
	if v, err := CoerceWriteValue(ks, true); err != nil || v != true {
		t.Errorf("bool coerce: %v %v", v, err)
	}
	if _, err := CoerceWriteValue(ks, "true"); err == nil {
		t.Errorf("bool coerce of string: want error")
	}
	intF := Feature{Key: "i", Type: TypeInt}
	if v, err := CoerceWriteValue(intF, float64(7)); err != nil || v != 7 {
		t.Errorf("int coerce: %v %v", v, err)
	}
	enumF := Feature{Key: "e", Type: TypeEnum, EnumValues: []string{"a", "b"}}
	if v, err := CoerceWriteValue(enumF, "a"); err != nil || v != "a" {
		t.Errorf("enum coerce: %v %v", v, err)
	}
	if _, err := CoerceWriteValue(enumF, "z"); err == nil {
		t.Errorf("enum coerce invalid: want error")
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
