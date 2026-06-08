package viewermanager

import (
	"context"
	"testing"
)

// ---------- cloud_stream_profile (Cloud-Profil, Saison 19-47) ----------

// TestCloudStreamProfile_DefaultFallsBackToLAN: with no cloud override the
// cloud profile resolves to the SAME value as the LAN resolution - the
// non-breaking default (the cloud publish reused the LAN profile before the
// two-field split). Exercises the COALESCE -> empty -> fallback wiring.
func TestCloudStreamProfile_DefaultFallsBackToLAN(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	info, err := mgr.GetViewerInfo(context.Background(), spec.MAC)
	if err != nil {
		t.Fatalf("GetViewerInfo: %v", err)
	}
	if got, want := info.ResolveCloudStreamProfile(), info.ResolveStreamProfile(); got != want {
		t.Errorf("default ResolveCloudStreamProfile = %q, want LAN fallback %q", got, want)
	}
}

// TestSetCloudStreamProfile_RoundTrip: an explicit cloud override is stored
// and returned by ResolveCloudStreamProfile WITHOUT touching the LAN profile;
// clearing it (empty) falls back to the LAN resolution again.
func TestSetCloudStreamProfile_RoundTrip(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	info, _ := mgr.GetViewerInfo(context.Background(), spec.MAC)
	lanBefore := info.ResolveStreamProfile()

	if err := mgr.SetCloudStreamProfile(context.Background(), spec.MAC, "intercom_med"); err != nil {
		t.Fatalf("SetCloudStreamProfile: %v", err)
	}
	info, _ = mgr.GetViewerInfo(context.Background(), spec.MAC)
	if got := info.ResolveCloudStreamProfile(); got != "intercom_med" {
		t.Errorf("cloud profile after set = %q, want intercom_med", got)
	}
	// LAN profile is independent - the cloud override must not change it.
	if got := info.ResolveStreamProfile(); got != lanBefore {
		t.Errorf("LAN profile changed to %q, want unchanged %q", got, lanBefore)
	}

	// Clearing the override restores the LAN fallback.
	if err := mgr.SetCloudStreamProfile(context.Background(), spec.MAC, ""); err != nil {
		t.Fatalf("SetCloudStreamProfile(empty): %v", err)
	}
	info, _ = mgr.GetViewerInfo(context.Background(), spec.MAC)
	if got, want := info.ResolveCloudStreamProfile(), info.ResolveStreamProfile(); got != want {
		t.Errorf("after clear = %q, want LAN fallback %q", got, want)
	}
}
