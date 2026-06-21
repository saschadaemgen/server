// Package featuregate is the Saison-20 feature-gating resolution layer.
//
// The CATALOG (DefaultCatalog) is the source of truth: every gateable
// function, its value type, its catalog defaults (active / licensed) and the
// BRIDGE from the generic string key to the existing typed viewers.* column
// via the proven viewermanager.Resolve*() methods.
//
// Resolve is a PURE function: given a Feature, a Snapshot (license + optional
// template + per-viewer active overrides) and the already-loaded ViewerInfo,
// it returns the Effective {licensed, active, value} for one (function,
// viewer). Precedence:
//
//	licensed = license_features row ?? catalog DefaultLicensed
//	active   = viewer override ?? template ?? catalog DefaultActive
//	value    = viewer column (set) ?? template value ?? catalog/type default
//
// The package imports viewermanager ONE WAY (for the ViewerInfo bridge); the
// manager never imports featuregate - it stays write+notify. The value bridge
// DELEGATES to the existing Resolve*() so the viewer-type-dependent keep_stream
// default is preserved, not reimplemented.
package featuregate

import (
	"fmt"
	"strconv"
	"strings"

	"carvilon.local/server/internal/viewermanager"
)

// FeatureType is the value type of a function. It drives how a template's
// generic TEXT value is parsed back into a typed value.
type FeatureType int

const (
	TypeBool FeatureType = iota
	TypeInt
	TypeEnum
	TypeString
)

// Feature describes one gateable function. Catalog entries are treated as
// immutable; callers read them, never mutate.
type Feature struct {
	Key             string
	Type            FeatureType
	EnumValues      []string // only TypeEnum
	DefaultActive   bool
	DefaultLicensed bool
	// Column is the viewers.* column this function maps to. Informational
	// here - viewer-value writes still go through the viewermanager setters.
	Column string

	// ViewerValueSet reports whether the underlying viewer column holds an
	// explicit value (non-NULL / non-empty) that must win over a template.
	ViewerValueSet func(*viewermanager.ViewerInfo) bool
	// ResolveValue returns the existing Resolve*() result: the set column
	// value, or the (possibly viewer-type dependent) default when unset.
	ResolveValue func(*viewermanager.ViewerInfo) any
	// ParseValue parses a template's generic TEXT value into the typed value.
	ParseValue func(string) (any, error)
}

// Effective is the resolved gate for one (function, viewer).
type Effective struct {
	Licensed bool
	Active   bool
	// Value is the resolved typed value when Licensed; nil when locked.
	Value any
}

// Bool returns the Effective value as a bool, or def when it is not a bool
// (locked function, or a non-bool feature). Lets endpoints fall back to the
// existing Resolve*() value without a panicking type assertion.
func (e Effective) Bool(def bool) bool {
	if b, ok := e.Value.(bool); ok {
		return b
	}
	return def
}

// Gate is the additive, client-facing gating descriptor: the omitempty block
// added to /esp/config and /webviewer/settings.json. It carries NO value - the
// values stay in the existing flat fields (rollout 2a: no field weglassen).
type Gate struct {
	Licensed bool `json:"licensed"`
	Active   bool `json:"active"`
}

// parseBool / parseInt / parseString / parseEnum turn a template's generic
// TEXT value into the catalog's typed value. A parse failure is returned to
// the resolver, which then falls back defensively to the catalog default.
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
