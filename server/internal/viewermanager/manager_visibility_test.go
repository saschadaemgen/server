package viewermanager

import (
	"context"
	"testing"
)

// ---------- viewer_setting_visibility (Saison 19-39) ----------

func TestViewerSettingVisibility_RoundTrip(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}

	// No rows -> empty map (everything visible by default).
	got, err := mgr.ListViewerSettingVisibility(context.Background(), spec.MAC)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("initial visibility = %v, want empty (default visible)", got)
	}

	if err := mgr.SetViewerSettingVisibility(context.Background(), spec.MAC, "language", false); err != nil {
		t.Fatalf("Set language=false: %v", err)
	}
	if err := mgr.SetViewerSettingVisibility(context.Background(), spec.MAC, "clock_layout", true); err != nil {
		t.Fatalf("Set clock_layout=true: %v", err)
	}
	got, _ = mgr.ListViewerSettingVisibility(context.Background(), spec.MAC)
	if v, ok := got["language"]; !ok || v {
		t.Errorf("language = (%v,%v), want (false,true)", v, ok)
	}
	if v, ok := got["clock_layout"]; !ok || !v {
		t.Errorf("clock_layout = (%v,%v), want (true,true)", v, ok)
	}

	// Upsert flips it (no duplicate row).
	if err := mgr.SetViewerSettingVisibility(context.Background(), spec.MAC, "language", true); err != nil {
		t.Fatalf("Set language=true: %v", err)
	}
	got, _ = mgr.ListViewerSettingVisibility(context.Background(), spec.MAC)
	if !got["language"] {
		t.Errorf("after flip language = %v, want true", got["language"])
	}
	if len(got) != 2 {
		t.Errorf("rows = %d, want 2 (upsert, not insert)", len(got))
	}
}

// FK ON DELETE CASCADE: removing the viewer purges its visibility rows.
func TestViewerSettingVisibility_CascadeOnDelete(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	if err := mgr.SetViewerSettingVisibility(context.Background(), spec.MAC, "language", false); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := mgr.RemoveViewer(context.Background(), spec.MAC); err != nil {
		t.Fatalf("RemoveViewer: %v", err)
	}
	got, err := mgr.ListViewerSettingVisibility(context.Background(), spec.MAC)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("visibility rows after viewer delete = %v, want empty (cascade)", got)
	}
}
