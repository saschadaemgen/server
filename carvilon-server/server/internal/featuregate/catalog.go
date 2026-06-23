package featuregate

import (
	"context"
	"fmt"

	"carvilon.local/server/internal/viewermanager"
)

// Feature keys - stable wire/db identifiers.
//
// keep_stream is registered as TWO atomic functions (Festlegung 5), each a 1:1
// bridge to one viewers.* column, FULLY wired (value + write). The 6 legacy
// settings are registered exposure-only (Festlegung 7): the catalog is the
// single registry of gateable keys, but their value/write bridges stay in the
// handlers until "Web dran ist".
const (
	KeyKeepStreamInScreensaver = "keep_stream_in_screensaver"
	KeyKeepStreamInScreenOff   = "keep_stream_in_screen_off"

	KeyIdleViewMode           = "idle_view_mode"
	KeyAutoScreensaverSeconds = "auto_screensaver_seconds"
	KeyClockLayout            = "clock_layout"
	KeyLanguage               = "language"
	KeyHistoryCaptureEnabled  = "history_capture_enabled"
	KeyResolutionMode         = "resolution_mode"
	// KeyPathMode (Verbindungsweg) joins the catalog in S20 so its row carries
	// the exposure controls too. Exposure-only (value stays in the handler);
	// migration 029 made viewers.path_mode nullable so it can inherit.
	KeyPathMode = "path_mode"
)

// catalog is the function registry, built once. Closures capture nothing, so a
// single shared instance is safe; callers treat entries as read-only.
var catalog = []Feature{
	{
		Key:             KeyKeepStreamInScreensaver,
		Type:            TypeBool,
		DefaultLicensed: true,
		Column:          "keep_stream_in_screensaver",
		ViewerValueSet: func(v *viewermanager.ViewerInfo) bool {
			return v != nil && v.KeepStreamInScreensaver != nil
		},
		ResolveValue: func(v *viewermanager.ViewerInfo) any {
			return v.ResolveKeepStreamInScreensaver()
		},
		DefaultValue: func(v *viewermanager.ViewerInfo) any {
			// Override-ignoring default: clear the column on a copy and reuse
			// the proven Resolve*() so the viewer-type default is not rebuilt.
			clone := *v
			clone.KeepStreamInScreensaver = nil
			return clone.ResolveKeepStreamInScreensaver()
		},
		ParseValue: parseBool,
		Write: func(ctx context.Context, mgr *viewermanager.Manager, mac string, value any) error {
			b, ok := value.(bool)
			if !ok {
				return fmt.Errorf("featuregate: %s: want bool, got %T", KeyKeepStreamInScreensaver, value)
			}
			return mgr.SetKeepStreamInScreensaver(ctx, mac, b)
		},
	},
	{
		Key:             KeyKeepStreamInScreenOff,
		Type:            TypeBool,
		DefaultLicensed: true,
		Column:          "keep_stream_in_screen_off",
		ViewerValueSet: func(v *viewermanager.ViewerInfo) bool {
			return v != nil && v.KeepStreamInScreenOff != nil
		},
		ResolveValue: func(v *viewermanager.ViewerInfo) any {
			return v.ResolveKeepStreamInScreenOff()
		},
		DefaultValue: func(v *viewermanager.ViewerInfo) any {
			clone := *v
			clone.KeepStreamInScreenOff = nil
			return clone.ResolveKeepStreamInScreenOff()
		},
		ParseValue: parseBool,
		Write: func(ctx context.Context, mgr *viewermanager.Manager, mac string, value any) error {
			b, ok := value.(bool)
			if !ok {
				return fmt.Errorf("featuregate: %s: want bool, got %T", KeyKeepStreamInScreenOff, value)
			}
			return mgr.SetKeepStreamInScreenOff(ctx, mac, b)
		},
	},

	// --- Legacy 6: exposure-only registration (no value/write bridge yet) ---
	{Key: KeyIdleViewMode, Type: TypeEnum, DefaultLicensed: true, Column: "idle_view_mode"},
	{Key: KeyAutoScreensaverSeconds, Type: TypeInt, DefaultLicensed: true, Column: "auto_screensaver_seconds"},
	{Key: KeyClockLayout, Type: TypeEnum, DefaultLicensed: true, Column: "clock_layout"},
	{Key: KeyLanguage, Type: TypeEnum, DefaultLicensed: true, Column: "language"},
	{Key: KeyHistoryCaptureEnabled, Type: TypeBool, DefaultLicensed: true, Column: "history_capture_enabled"},
	{Key: KeyResolutionMode, Type: TypeEnum, DefaultLicensed: true, Column: "resolution_mode"},
	{Key: KeyPathMode, Type: TypeEnum, DefaultLicensed: true, Column: "path_mode"},
}

var catalogByKey = func() map[string]Feature {
	m := make(map[string]Feature, len(catalog))
	for _, f := range catalog {
		m[f.Key] = f
	}
	return m
}()

// DefaultCatalog returns the function catalog - the source of truth. The
// returned slice is shared and read-only.
func DefaultCatalog() []Feature { return catalog }

// Lookup returns the catalog feature for key (found=false for unknown keys).
func Lookup(key string) (Feature, bool) {
	f, ok := catalogByKey[key]
	return f, ok
}
