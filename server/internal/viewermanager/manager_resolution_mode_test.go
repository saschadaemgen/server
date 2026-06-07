package viewermanager

import (
	"context"
	"testing"
)

// ---------- resolution_mode (Auflösungs-Wahl, Saison 19-42) ----------

func TestResolutionMode_DefaultMedium(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	info, err := mgr.GetViewerInfo(context.Background(), spec.MAC)
	if err != nil {
		t.Fatalf("GetViewerInfo: %v", err)
	}
	if got := info.ResolveResolutionMode(); got != ResolutionModeMedium {
		t.Errorf("default resolution_mode = %q, want %q", got, ResolutionModeMedium)
	}
}

func TestSetResolutionMode_WhitelistRoundTrip(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}

	for _, v := range []string{ResolutionModeHigh, ResolutionModeLow, ResolutionModeMedium} {
		if err := mgr.SetResolutionMode(context.Background(), spec.MAC, v); err != nil {
			t.Fatalf("SetResolutionMode(%q): %v", v, err)
		}
		info, _ := mgr.GetViewerInfo(context.Background(), spec.MAC)
		if got := info.ResolveResolutionMode(); got != v {
			t.Errorf("after SetResolutionMode(%q) = %q", v, got)
		}
	}

	// Garbage is rejected (whitelist).
	if err := mgr.SetResolutionMode(context.Background(), spec.MAC, "ultra"); err == nil {
		t.Error("SetResolutionMode(ultra) = nil, want error")
	}

	// Empty normalises to medium.
	if err := mgr.SetResolutionMode(context.Background(), spec.MAC, ""); err != nil {
		t.Fatalf("SetResolutionMode(empty): %v", err)
	}
	info, _ := mgr.GetViewerInfo(context.Background(), spec.MAC)
	if info.ResolveResolutionMode() != ResolutionModeMedium {
		t.Errorf("SetResolutionMode(empty) -> %q, want medium", info.ResolveResolutionMode())
	}
}
