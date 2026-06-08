package viewermanager

import (
	"context"
	"testing"
)

// ---------- viewer_doors 1:n assignment (Saison 19-30) ----------

func TestViewerDoors_SetListRoundTrip(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	want := []DoorAssignment{
		{DoorID: "door-uuid-front", Label: "Haupteingang", Sort: 0},
		{DoorID: "door-uuid-back", Label: "", Sort: 1},
	}
	if err := mgr.SetViewerDoors(context.Background(), spec.MAC, want); err != nil {
		t.Fatalf("SetViewerDoors: %v", err)
	}
	got, err := mgr.ListViewerDoors(context.Background(), spec.MAC)
	if err != nil {
		t.Fatalf("ListViewerDoors: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListViewerDoors len = %d, want 2", len(got))
	}
	if got[0].DoorID != "door-uuid-front" || got[0].Label != "Haupteingang" {
		t.Errorf("door[0] = %+v, want front/Haupteingang", got[0])
	}
	if got[1].DoorID != "door-uuid-back" {
		t.Errorf("door[1] = %+v, want back", got[1])
	}
}

func TestViewerDoors_HasDoorAuthz(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	if err := mgr.AddViewerDoor(context.Background(), spec.MAC,
		DoorAssignment{DoorID: "door-assigned"}); err != nil {
		t.Fatalf("AddViewerDoor: %v", err)
	}

	has, err := mgr.ViewerHasDoor(context.Background(), spec.MAC, "door-assigned")
	if err != nil {
		t.Fatalf("ViewerHasDoor assigned: %v", err)
	}
	if !has {
		t.Error("ViewerHasDoor(assigned) = false, want true")
	}
	has, err = mgr.ViewerHasDoor(context.Background(), spec.MAC, "door-stranger")
	if err != nil {
		t.Fatalf("ViewerHasDoor stranger: %v", err)
	}
	if has {
		t.Error("ViewerHasDoor(stranger) = true, want false (authz leak)")
	}
}

func TestViewerDoors_SetEmptyClears(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	if err := mgr.SetViewerDoors(context.Background(), spec.MAC,
		[]DoorAssignment{{DoorID: "door-x"}}); err != nil {
		t.Fatalf("SetViewerDoors: %v", err)
	}
	if err := mgr.SetViewerDoors(context.Background(), spec.MAC, nil); err != nil {
		t.Fatalf("SetViewerDoors clear: %v", err)
	}
	got, err := mgr.ListViewerDoors(context.Background(), spec.MAC)
	if err != nil {
		t.Fatalf("ListViewerDoors: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListViewerDoors after clear = %d, want 0", len(got))
	}
}

func TestViewerDoors_RemoveOne(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	if err := mgr.SetViewerDoors(context.Background(), spec.MAC, []DoorAssignment{
		{DoorID: "door-a"}, {DoorID: "door-b"},
	}); err != nil {
		t.Fatalf("SetViewerDoors: %v", err)
	}
	if err := mgr.RemoveViewerDoor(context.Background(), spec.MAC, "door-a"); err != nil {
		t.Fatalf("RemoveViewerDoor: %v", err)
	}
	got, _ := mgr.ListViewerDoors(context.Background(), spec.MAC)
	if len(got) != 1 || got[0].DoorID != "door-b" {
		t.Errorf("after remove = %+v, want [door-b]", got)
	}
}

// TestViewerDoors_CascadeOnViewerDelete proves the FK ON DELETE
// CASCADE: removing the viewer purges its door assignments (no
// orphan rows survive).
func TestViewerDoors_CascadeOnViewerDelete(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	if err := mgr.AddViewerDoor(context.Background(), spec.MAC,
		DoorAssignment{DoorID: "door-a"}); err != nil {
		t.Fatalf("AddViewerDoor: %v", err)
	}
	if err := mgr.RemoveViewer(context.Background(), spec.MAC); err != nil {
		t.Fatalf("RemoveViewer: %v", err)
	}
	got, err := mgr.ListViewerDoors(context.Background(), spec.MAC)
	if err != nil {
		t.Fatalf("ListViewerDoors: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("door assignments after viewer delete = %d, want 0 (cascade)", len(got))
	}
}
