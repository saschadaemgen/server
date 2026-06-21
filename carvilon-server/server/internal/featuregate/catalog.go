package featuregate

import "carvilon.local/server/internal/viewermanager"

// Feature keys - stable wire/db identifiers. keep_stream is registered as TWO
// atomic functions (Festlegung 5): each is a 1:1 bridge to exactly one
// viewers.* column, no special multi-column case.
const (
	KeyKeepStreamInScreensaver = "keep_stream_in_screensaver"
	KeyKeepStreamInScreenOff   = "keep_stream_in_screen_off"
)

// catalog is the function registry, built once. Closures capture nothing, so a
// single shared instance is safe; callers must treat entries as read-only.
var catalog = []Feature{
	{
		Key:             KeyKeepStreamInScreensaver,
		Type:            TypeBool,
		DefaultActive:   true,
		DefaultLicensed: true,
		Column:          "keep_stream_in_screensaver",
		ViewerValueSet: func(v *viewermanager.ViewerInfo) bool {
			return v != nil && v.KeepStreamInScreensaver != nil
		},
		ResolveValue: func(v *viewermanager.ViewerInfo) any {
			return v.ResolveKeepStreamInScreensaver()
		},
		ParseValue: parseBool,
	},
	{
		Key:             KeyKeepStreamInScreenOff,
		Type:            TypeBool,
		DefaultActive:   true,
		DefaultLicensed: true,
		Column:          "keep_stream_in_screen_off",
		ViewerValueSet: func(v *viewermanager.ViewerInfo) bool {
			return v != nil && v.KeepStreamInScreenOff != nil
		},
		ResolveValue: func(v *viewermanager.ViewerInfo) any {
			return v.ResolveKeepStreamInScreenOff()
		},
		ParseValue: parseBool,
	},
}

// DefaultCatalog returns the function catalog - the source of truth. The
// returned slice is shared and read-only.
func DefaultCatalog() []Feature { return catalog }
