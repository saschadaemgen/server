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

// End-to-end through the store: a template sets exposure+value, the viewer
// exposure override wins, and resolution reflects it.
func TestStore_SnapshotForViewer_EndToEnd(t *testing.T) {
	st, d := newTestStore(t)
	ctx := context.Background()
	insertViewer(t, d, "AA:BB", viewermanager.TypeAndroid, 9001)

	snap, err := st.SnapshotForViewer(ctx, "AA:BB")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Template != nil {
		t.Errorf("template = %+v, want nil", snap.Template)
	}

	// Template: keep_stream value=false + exposure=admin_only.
	id, err := st.CreateTemplate(ctx, "Sparmodus")
	if err != nil {
		t.Fatalf("create template: %v", err)
	}
	if err := st.SetTemplateFeature(ctx, id, KeyKeepStreamInScreensaver, strPtr(ExposureAdminOnly), strPtr("false")); err != nil {
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
		t.Errorf("template value should override android default -> false, got %v", eff.Value)
	}
	if eff.Exposure != ExposureAdminOnly || eff.Writable {
		t.Errorf("template exposure admin_only: got %+v, want admin_only + not writable", eff)
	}

	// Viewer exposure override (tenant_visible) wins over the template's admin_only.
	if err := st.SetViewerExposure(ctx, "AA:BB", KeyKeepStreamInScreensaver, ExposureTenantVisible); err != nil {
		t.Fatalf("set viewer exposure: %v", err)
	}
	snap, _ = st.SnapshotForViewer(ctx, "AA:BB")
	if eff := Resolve(feat(KeyKeepStreamInScreensaver), snap, info); !eff.Writable || eff.Exposure != ExposureTenantVisible {
		t.Errorf("viewer override tenant_visible should win, got %+v", eff)
	}
}

func TestStore_SetViewerExposure_RejectsInvalid(t *testing.T) {
	st, d := newTestStore(t)
	insertViewer(t, d, "EE:FF", viewermanager.TypeAndroid, 9050)
	// Saison 20: bookable is now an accepted state (resolves like hidden).
	if err := st.SetViewerExposure(context.Background(), "EE:FF", KeyKeepStreamInScreensaver, "bookable"); err != nil {
		t.Errorf("SetViewerExposure(bookable): want accepted, got %v", err)
	}
	if err := st.SetViewerExposure(context.Background(), "EE:FF", KeyKeepStreamInScreensaver, "nonsense"); err == nil {
		t.Errorf("SetViewerExposure(nonsense): want error")
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
	eff := Resolve(feat(KeyKeepStreamInScreensaver), snap, &viewermanager.ViewerInfo{Type: viewermanager.TypeAndroid})
	if eff.Licensed || eff.Writable {
		t.Errorf("locked feature: got %+v, want not licensed/not writable", eff)
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
