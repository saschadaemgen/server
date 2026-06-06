package viewermanager

import (
	"context"
	"testing"
)

// ---------- path_mode (WEG-Schalter, Saison 19-39) ----------

func TestPathMode_DefaultAuto(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	info, err := mgr.GetViewerInfo(context.Background(), spec.MAC)
	if err != nil {
		t.Fatalf("GetViewerInfo: %v", err)
	}
	if got := info.ResolvePathMode(); got != PathModeAuto {
		t.Errorf("default path_mode = %q, want %q", got, PathModeAuto)
	}
}

func TestSetPathMode_WhitelistRoundTrip(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}

	for _, v := range []string{PathModeLocal, PathModeCloud, PathModeAuto} {
		if err := mgr.SetPathMode(context.Background(), spec.MAC, v); err != nil {
			t.Fatalf("SetPathMode(%q): %v", v, err)
		}
		info, _ := mgr.GetViewerInfo(context.Background(), spec.MAC)
		if got := info.ResolvePathMode(); got != v {
			t.Errorf("after SetPathMode(%q) = %q", v, got)
		}
	}

	// Garbage is rejected (whitelist).
	if err := mgr.SetPathMode(context.Background(), spec.MAC, "garbage"); err == nil {
		t.Error("SetPathMode(garbage) = nil, want error")
	}

	// Empty normalises to auto.
	if err := mgr.SetPathMode(context.Background(), spec.MAC, ""); err != nil {
		t.Fatalf("SetPathMode(empty): %v", err)
	}
	info, _ := mgr.GetViewerInfo(context.Background(), spec.MAC)
	if info.ResolvePathMode() != PathModeAuto {
		t.Errorf("SetPathMode(empty) -> %q, want auto", info.ResolvePathMode())
	}
}
