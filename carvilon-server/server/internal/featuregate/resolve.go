package featuregate

import "carvilon.local/server/internal/viewermanager"

// License is an immutable snapshot of the license_features rows. Licensed
// applies the rule "a row overrides the catalog default; row absence = catalog
// default" (Festlegung 4: zero behaviour break today, gesperrt only what is
// deliberately entered).
type License struct {
	features map[string]bool
}

// Licensed reports whether the function is unlocked. catalogDefault is the
// Feature.DefaultLicensed used when no license_features row exists.
func (l License) Licensed(key string, catalogDefault bool) bool {
	if l.features != nil {
		if v, ok := l.features[key]; ok {
			return v
		}
	}
	return catalogDefault
}

// Template is an immutable snapshot of one template's template_features rows.
// Only EXPLICIT (non-NULL) cells populate the maps; a missing key inherits.
// All methods are nil-receiver safe so "no template" needs no special-casing.
type Template struct {
	active map[string]bool   // rows where active IS NOT NULL
	value  map[string]string // rows where value  IS NOT NULL
}

// Active returns the template's active override for key, if one is set.
func (t *Template) Active(key string) (bool, bool) {
	if t == nil {
		return false, false
	}
	v, ok := t.active[key]
	return v, ok
}

// HasValue reports whether the template carries a value for key.
func (t *Template) HasValue(key string) bool {
	if t == nil {
		return false
	}
	_, ok := t.value[key]
	return ok
}

// RawValue returns the template's generic TEXT value for key ("" if none).
func (t *Template) RawValue(key string) string {
	if t == nil {
		return ""
	}
	return t.value[key]
}

// Snapshot bundles everything Resolve needs for one viewer: the license, the
// viewer's template (nil = none) and the per-viewer active overrides.
type Snapshot struct {
	License   License
	Template  *Template       // nil when the viewer has no template
	Overrides map[string]bool // viewer_feature_active: feature_key -> active
}

// Resolve computes the Effective gate for one function + viewer. Pure: no I/O,
// no globals beyond the passed Feature.
func Resolve(f Feature, snap Snapshot, info *viewermanager.ViewerInfo) Effective {
	// 1) License gate. Not licensed -> locked, no value.
	if !snap.License.Licensed(f.Key, f.DefaultLicensed) {
		return Effective{Licensed: false, Active: false, Value: nil}
	}

	// 2) active = viewer override ?? template ?? catalog default
	active := f.DefaultActive
	if a, ok := snap.Template.Active(f.Key); ok {
		active = a
	}
	if snap.Overrides != nil {
		if a, ok := snap.Overrides[f.Key]; ok {
			active = a
		}
	}

	// 3) value = viewer column (set) ?? template value ?? catalog/type default.
	// ResolveValue is the existing Resolve*(): in the "set" branch it returns
	// the explicit column value, in the "default" branch the (type-dependent)
	// default - so the proven keep_stream policy is reused, not rebuilt.
	var value any
	switch {
	case info != nil && f.ViewerValueSet != nil && f.ViewerValueSet(info):
		value = resolveDefault(f, info)
	case snap.Template.HasValue(f.Key):
		if v, err := f.ParseValue(snap.Template.RawValue(f.Key)); err == nil {
			value = v
		} else {
			// Defensive: a malformed template value never breaks delivery.
			value = resolveDefault(f, info)
		}
	default:
		value = resolveDefault(f, info)
	}
	return Effective{Licensed: true, Active: active, Value: value}
}

func resolveDefault(f Feature, info *viewermanager.ViewerInfo) any {
	if f.ResolveValue == nil {
		return nil
	}
	return f.ResolveValue(info)
}

// ResolveAll resolves every catalog function for one viewer.
func ResolveAll(cat []Feature, snap Snapshot, info *viewermanager.ViewerInfo) map[string]Effective {
	out := make(map[string]Effective, len(cat))
	for _, f := range cat {
		out[f.Key] = Resolve(f, snap, info)
	}
	return out
}

// GateMap projects a resolved set into the additive client gating block
// (licensed + active per key; no values). Returns nil for an empty set so the
// caller's omitempty drops the key.
func GateMap(gates map[string]Effective) map[string]Gate {
	if len(gates) == 0 {
		return nil
	}
	out := make(map[string]Gate, len(gates))
	for k, e := range gates {
		out[k] = Gate{Licensed: e.Licensed, Active: e.Active}
	}
	return out
}
