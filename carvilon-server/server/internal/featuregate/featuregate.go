// Package featuregate is the Saison-20 feature-gating resolution layer
// (Variante A: one three-level exposure per (viewer, function)).
//
// The CATALOG (DefaultCatalog) is the source of truth: every gateable
// function, its value type, its catalog defaults (licensed), and the BRIDGE
// from the generic string key to the existing typed viewers.* column via the
// proven viewermanager.Resolve*() methods (read) and Set*() methods (write).
//
// Resolve is a PURE function: given a Feature, a Snapshot (license + optional
// template + per-viewer exposure overrides) and the already-loaded ViewerInfo,
// it returns the Effective {licensed, exposure, value, writable} for one
// (function, viewer). Precedence:
//
//	licensed = license_features row ?? catalog DefaultLicensed
//	exposure = viewer override ?? template ?? tenant_visible
//	value    = hidden -> catalog/type default (override ignored);
//	           else viewer column (set) ?? template value ?? catalog/type default
//	writable = licensed && exposure == tenant_visible && has write bridge
//
// exposure is a free TEXT value, validated here against KnownExposures (no DB
// CHECK), so a future value like ExposureBookable can be added with NO
// migration. The value bridge DELEGATES to the existing Resolve*() so the
// viewer-type-dependent keep_stream default is preserved, not reimplemented.
// The package imports viewermanager ONE WAY; the manager never imports it.
package featuregate

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"carvilon.local/server/internal/viewermanager"
)

// Exposure levels. Free TEXT in the DB, validated in Go.
//
// From the tenant/app view: tenant_visible -> show + edit + write-back;
// admin_only and hidden -> not shown at all (the difference is only whether the
// admin may still set the value). ExposureBookable is the RESERVED second
// "hidden" variant (paid, not yet booked) - NOT built in this step, no logic,
// no offer/price/request. It is listed so the Go value space (and thus the wire
// format) already has the slot; adding it later costs no migration.
const (
	ExposureTenantVisible = "tenant_visible"
	ExposureAdminOnly     = "admin_only"
	ExposureHidden        = "hidden"
	// ExposureBookable is reserved; intentionally NOT in KnownExposures yet.
	ExposureBookable = "bookable"

	// DefaultExposure applies when neither viewer nor template sets a row.
	DefaultExposure = ExposureTenantVisible
)

// KnownExposures is the set accepted today (admin/seed writes are validated
// against it). ExposureBookable is deliberately absent until the abo/license
// server gives it meaning.
var KnownExposures = map[string]bool{
	ExposureTenantVisible: true,
	ExposureAdminOnly:     true,
	ExposureHidden:        true,
}

// ValidExposure reports whether s is an accepted exposure value today.
func ValidExposure(s string) bool { return KnownExposures[s] }

// FeatureType is the value type of a function. It drives how a template's
// generic TEXT value is parsed back into a typed value.
type FeatureType int

const (
	TypeBool FeatureType = iota
	TypeInt
	TypeEnum
	TypeString
)

// Feature describes one gateable function. Catalog entries are read-only.
//
// keep_stream functions are FULLY wired (ResolveValue + DefaultValue + Write +
// ParseValue). The 6 legacy settings are registered exposure-only (no value /
// write bridge yet) so the catalog is the single registry of gateable keys
// while their value path stays in the handlers ("Web spaeter").
type Feature struct {
	Key             string
	Type            FeatureType
	EnumValues      []string // only TypeEnum
	DefaultLicensed bool
	// Column is the viewers.* column this function maps to. Informational.
	Column string

	// ViewerValueSet reports whether the viewer column holds an explicit value
	// (non-NULL) that must win over a template. nil = no value bridge.
	ViewerValueSet func(*viewermanager.ViewerInfo) bool
	// ResolveValue returns the existing Resolve*() result: the set column value,
	// or the (type-dependent) default when unset. nil = no value bridge.
	ResolveValue func(*viewermanager.ViewerInfo) any
	// DefaultValue returns the catalog/type default IGNORING any viewer/template
	// override (used for exposure==hidden, and as the parse-error fallback). It
	// reuses the Resolve*() policy on a column-cleared view, so the keep_stream
	// type default is not rebuilt. nil = no value bridge.
	DefaultValue func(*viewermanager.ViewerInfo) any
	// ParseValue parses a template's / write-back's generic TEXT value into the
	// typed value. nil = no value bridge.
	ParseValue func(string) (any, error)
	// Write persists a tenant-supplied value through the existing manager
	// setter. nil = NOT tenant-writable via the JSON write-back (the function
	// can still be exposure-gated and read; it just never reports writable).
	Write func(ctx context.Context, mgr *viewermanager.Manager, mac string, value any) error
}

// Effective is the resolved gate for one (function, viewer).
type Effective struct {
	Licensed bool
	Exposure string // tenant_visible | admin_only | hidden
	// Value is the resolved typed value when Licensed; nil when locked.
	Value any
	// Writable is the SINGLE app-facing decision and is IDENTICAL to the
	// server's write-back accept rule: licensed && exposure==tenant_visible &&
	// the function has a write bridge.
	Writable bool
}

// TenantVisible reports whether the tenant sees the control (exposure ==
// tenant_visible). The web-compat visibility block derives from this.
func (e Effective) TenantVisible() bool { return e.Exposure == ExposureTenantVisible }

// Bool returns the Effective value as a bool, or def when it is not a bool
// (locked function, or a non-bool feature). Lets endpoints fall back to the
// existing Resolve*() value without a panicking type assertion.
func (e Effective) Bool(def bool) bool {
	if b, ok := e.Value.(bool); ok {
		return b
	}
	return def
}

// Gate is the additive, client-facing gating descriptor: per function key
// {licensed, exposure, writable}. It carries NO value - the values stay in the
// existing flat fields (rollout 2a: no field weglassen).
type Gate struct {
	Licensed bool   `json:"licensed"`
	Exposure string `json:"exposure"`
	Writable bool   `json:"writable"`
}

// parseBool / parseInt / parseString / parseEnum turn a generic TEXT value into
// the catalog's typed value. A parse failure is returned to the resolver, which
// then falls back defensively to the catalog default.
func parseBool(s string) (any, error) {
	b, err := strconv.ParseBool(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("featuregate: parse bool %q: %w", s, err)
	}
	return b, nil
}

func parseInt(s string) (any, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("featuregate: parse int %q: %w", s, err)
	}
	return n, nil
}

func parseString(s string) (any, error) { return s, nil }

// CoerceWriteValue normalises a JSON-decoded value (bool / float64 / string) to
// the catalog type for a write-back, validating enums. Used by the write-back
// endpoint before calling Feature.Write.
func CoerceWriteValue(f Feature, v any) (any, error) {
	switch f.Type {
	case TypeBool:
		if b, ok := v.(bool); ok {
			return b, nil
		}
		return nil, fmt.Errorf("featuregate: %s: want bool, got %T", f.Key, v)
	case TypeInt:
		switch n := v.(type) {
		case float64:
			return int(n), nil
		case int:
			return n, nil
		}
		return nil, fmt.Errorf("featuregate: %s: want number, got %T", f.Key, v)
	case TypeEnum:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("featuregate: %s: want string, got %T", f.Key, v)
		}
		for _, a := range f.EnumValues {
			if a == s {
				return s, nil
			}
		}
		return nil, fmt.Errorf("featuregate: %s: %q not in %v", f.Key, s, f.EnumValues)
	case TypeString:
		if s, ok := v.(string); ok {
			return s, nil
		}
		return nil, fmt.Errorf("featuregate: %s: want string, got %T", f.Key, v)
	}
	return nil, fmt.Errorf("featuregate: %s: unsupported type", f.Key)
}

func parseEnum(allowed []string) func(string) (any, error) {
	return func(s string) (any, error) {
		t := strings.TrimSpace(s)
		for _, a := range allowed {
			if a == t {
				return t, nil
			}
		}
		return nil, fmt.Errorf("featuregate: value %q not in enum %v", s, allowed)
	}
}
