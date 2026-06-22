package featuregate

import "carvilon.local/server/internal/viewermanager"

// License is an immutable snapshot of the license_features rows. Licensed
// applies the rule "a row overrides the catalog default; row absence = catalog
// default" (zero behaviour break today; gesperrt only what is deliberately
// entered).
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
	exposure map[string]string // rows where exposure IS NOT NULL
	value    map[string]string // rows where value    IS NOT NULL
}

// Exposure returns the template's exposure override for key, if one is set.
func (t *Template) Exposure(key string) (string, bool) {
	if t == nil {
		return "", false
	}
	v, ok := t.exposure[key]
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
// viewer's template (nil = none) and the per-viewer exposure overrides.
type Snapshot struct {
	License   License
	Template  *Template         // nil when the viewer has no template
	Overrides map[string]string // viewer_feature_exposure: feature_key -> exposure
}

// Resolve computes the Effective gate for one function + viewer. Pure.
func Resolve(f Feature, snap Snapshot, info *viewermanager.ViewerInfo) Effective {
	licensed := snap.License.Licensed(f.Key, f.DefaultLicensed)

	// exposure = viewer override ?? template ?? tenant_visible. Unknown values
	// are ignored (defensive; validation happens on write).
	exposure := DefaultExposure
	if e, ok := snap.Template.Exposure(f.Key); ok && ValidExposure(e) {
		exposure = e
	}
	if snap.Overrides != nil {
		if e, ok := snap.Overrides[f.Key]; ok && ValidExposure(e) {
			exposure = e
		}
	}

	// value. Only meaningful when licensed; locked -> nil (the endpoint keeps
	// delivering the flat value via its own Resolve*() fallback, rollout 2a).
	var value any
	if licensed {
		switch {
		case exposure == ExposureHidden:
			// hidden forces the catalog/type default; any override is ignored.
			value = defaultValue(f, info)
		case info != nil && f.ViewerValueSet != nil && f.ViewerValueSet(info):
			value = resolveValue(f, info) // explicit viewer column wins
		case snap.Template.HasValue(f.Key) && f.ParseValue != nil:
			if v, err := f.ParseValue(snap.Template.RawValue(f.Key)); err == nil {
				value = v
			} else {
				value = defaultValue(f, info) // malformed template value -> default
			}
		default:
			value = defaultValue(f, info)
		}
	}

	// writable is the SINGLE app-facing decision AND the server write-back
	// accept rule: licensed && tenant_visible && a write bridge exists.
	writable := licensed && exposure == ExposureTenantVisible && f.Write != nil

	return Effective{Licensed: licensed, Exposure: exposure, Value: value, Writable: writable}
}

// resolveValue is the column-aware value (set column, or type default when
// unset). defaultValue is the override-ignoring catalog/type default.
func resolveValue(f Feature, info *viewermanager.ViewerInfo) any {
	if f.ResolveValue == nil {
		return nil
	}
	return f.ResolveValue(info)
}

func defaultValue(f Feature, info *viewermanager.ViewerInfo) any {
	if f.DefaultValue != nil {
		return f.DefaultValue(info)
	}
	// No override-ignoring bridge: fall back to the column-aware resolver (for
	// an unset column the two coincide).
	return resolveValue(f, info)
}

// ResolveAll resolves every catalog function for one viewer.
func ResolveAll(cat []Feature, snap Snapshot, info *viewermanager.ViewerInfo) map[string]Effective {
	out := make(map[string]Effective, len(cat))
	for _, f := range cat {
		out[f.Key] = Resolve(f, snap, info)
	}
	return out
}

// GatingBlock projects the resolved set into the additive client gating block
// {licensed, exposure, writable}, EMITTING ONLY write-back-capable functions
// (those with a write bridge). That keeps every emitted `writable` exactly
// equal to the server's write-back accept rule - one source, no drift. The
// exposure-only legacy keys surface via the derived visibility block instead.
// Returns nil for an empty set so the caller's omitempty drops the key.
func GatingBlock(cat []Feature, gates map[string]Effective) map[string]Gate {
	out := make(map[string]Gate)
	for _, f := range cat {
		if f.Write == nil {
			continue
		}
		e, ok := gates[f.Key]
		if !ok {
			continue
		}
		out[f.Key] = Gate{Licensed: e.Licensed, Exposure: e.Exposure, Writable: e.Writable}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
