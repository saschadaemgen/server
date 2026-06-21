package featuregate

import (
	"context"
	"path/filepath"
	"testing"

	"carvilon.local/server/internal/db"
	"carvilon.local/server/internal/viewermanager"
)

func newTestStore(t *testing.T) (*Store, *db.DB) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "fg.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return NewStore(d.DB), d
}

func insertViewer(t *testing.T, d *db.DB, mac, typ string, port int) {
	t.Helper()
	_, err := d.Exec(
		`INSERT INTO viewers (mac, name, service_port, type, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 0, 0)`, mac, "Test "+mac, port, typ)
	if err != nil {
		t.Fatalf("insert viewer %s: %v", mac, err)
	}
}

// End-to-end through the store: a template value overrides the (unset) viewer
// column, and the active axis resolves viewer-override > template > default.
func TestStore_SnapshotForViewer_EndToEnd(t *testing.T) {
	st, d := newTestStore(t)
	ctx := context.Background()
	insertViewer(t, d, "AA:BB", viewermanager.TypeAndroid, 9001)

	// No license rows, no template: license default true, no template, empty overrides.
	snap, err := st.SnapshotForViewer(ctx, "AA:BB")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Template != nil {
		t.Errorf("template = %+v, want nil", snap.Template)
	}
	if !snap.License.Licensed(KeyKeepStreamInScreensaver, true) {
		t.Errorf("license default: want true")
	}

	// Template sets value=false + active=false; attach it to the viewer.
	id, err := st.CreateTemplate(ctx, "Sparmodus")
	if err != nil {
		t.Fatalf("create template: %v", err)
	}
	if err := st.SetTemplateFeature(ctx, id, KeyKeepStreamInScreensaver, ptrBool(false), strPtr("false")); err != nil {
		t.Fatalf("set template feature: %v", err)
	}
	if err := st.AssignViewerTemplate(ctx, "AA:BB", &id); err != nil {
		t.Fatalf("assign template: %v", err)
	}

	snap, err = st.SnapshotForViewer(ctx, "AA:BB")
	if err != nil {
		t.Fatalf("snapshot 2: %v", err)
	}
	info := &viewermanager.ViewerInfo{Type: viewermanager.TypeAndroid} // column unset
	eff := Resolve(feat(KeyKeepStreamInScreensaver), snap, info)
	if eff.Value != false {
		t.Errorf("template value should override android default true -> false, got %v", eff.Value)
	}
	if eff.Active {
		t.Errorf("template active=false -> Active false, got true")
	}

	// Viewer override active=true wins over the template's active=false.
	if err := st.SetViewerFeatureActive(ctx, "AA:BB", KeyKeepStreamInScreensaver, true); err != nil {
		t.Fatalf("set viewer feature active: %v", err)
	}
	snap, _ = st.SnapshotForViewer(ctx, "AA:BB")
	if eff := Resolve(feat(KeyKeepStreamInScreensaver), snap, info); !eff.Active {
		t.Errorf("viewer override active=true should win, got Active false")
	}
}

func TestStore_LicenseFeatureLocks(t *testing.T) {
	st, d := newTestStore(t)
	ctx := context.Background()
	insertViewer(t, d, "CC:DD", viewermanager.TypeAndroid, 9002)

	if err := st.SetLicenseFeature(ctx, KeyKeepStreamInScreensaver, false); err != nil {
		t.Fatalf("set license feature: %v", err)
	}
	snap, err := st.SnapshotForViewer(ctx, "CC:DD")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.License.Licensed(KeyKeepStreamInScreensaver, true) {
		t.Errorf("license feature licensed=false not honoured")
	}
	eff := Resolve(feat(KeyKeepStreamInScreensaver), snap, &viewermanager.ViewerInfo{Type: viewermanager.TypeAndroid})
	if eff.Licensed {
		t.Errorf("locked feature still licensed")
	}
}

func TestStore_ViewersByTemplate(t *testing.T) {
	st, d := newTestStore(t)
	ctx := context.Background()
	insertViewer(t, d, "AA:01", viewermanager.TypeAndroid, 9100)
	insertViewer(t, d, "AA:02", viewermanager.TypeWeb, 9101)
	insertViewer(t, d, "AA:03", viewermanager.TypeESP, 9102)

	id, err := st.CreateTemplate(ctx, "T")
	if err != nil {
		t.Fatalf("create template: %v", err)
	}
	if err := st.AssignViewerTemplate(ctx, "AA:01", &id); err != nil {
		t.Fatalf("assign 01: %v", err)
	}
	if err := st.AssignViewerTemplate(ctx, "AA:03", &id); err != nil {
		t.Fatalf("assign 03: %v", err)
	}
	macs, err := st.ViewersByTemplate(ctx, id)
	if err != nil {
		t.Fatalf("viewers by template: %v", err)
	}
	if len(macs) != 2 || macs[0] != "AA:01" || macs[1] != "AA:03" {
		t.Errorf("macs = %v, want [AA:01 AA:03]", macs)
	}
}

func TestStore_AssignTemplateUnknownViewer(t *testing.T) {
	st, _ := newTestStore(t)
	id, err := st.CreateTemplate(context.Background(), "T")
	if err != nil {
		t.Fatalf("create template: %v", err)
	}
	if err := st.AssignViewerTemplate(context.Background(), "NOPE", &id); err == nil {
		t.Errorf("assign to unknown viewer: want error, got nil")
	}
}

func TestStore_SetLicense_Singleton(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	limit := 5
	if err := st.SetLicense(ctx, "basic", &limit, nil); err != nil {
		t.Fatalf("set license: %v", err)
	}
	limit2 := 10
	if err := st.SetLicense(ctx, "pro", &limit2, nil); err != nil {
		t.Fatalf("set license 2: %v", err)
	}
	// Singleton: still exactly one row, updated in place.
	var count, gotLimit int
	var plan string
	if err := st.db.QueryRowContext(ctx,
		`SELECT COUNT(*), MAX(plan_name), MAX(viewer_limit) FROM license`).Scan(&count, &plan, &gotLimit); err != nil {
		t.Fatalf("query license: %v", err)
	}
	if count != 1 || plan != "pro" || gotLimit != 10 {
		t.Errorf("license = (count=%d plan=%q limit=%d), want (1, pro, 10)", count, plan, gotLimit)
	}
}
