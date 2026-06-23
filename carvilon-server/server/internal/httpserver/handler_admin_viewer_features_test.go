package httpserver

import (
	"context"
	"net/http"
	"testing"

	"carvilon.local/server/internal/featuregate"
)

// TestAdminViewerExposure_SetsThreeLevel proves the Saison-20 three-level
// exposure endpoint writes the per-viewer override the resolver reads back.
func TestAdminViewerExposure_SetsThreeLevel(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	resp := postAdminViewerJSON(t, env, "/a/viewers/"+testViewerMAC+"/exposure", map[string]any{
		"feature_key": featuregate.KeyKeepStreamInScreensaver,
		"exposure":    featuregate.ExposureHidden,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}

	st := featuregate.NewStore(env.d.DB)
	snap, err := st.SnapshotForViewer(context.Background(), testViewerMAC)
	if err != nil {
		t.Fatalf("SnapshotForViewer: %v", err)
	}
	if got := snap.Overrides[featuregate.KeyKeepStreamInScreensaver]; got != featuregate.ExposureHidden {
		t.Errorf("override = %q, want %q", got, featuregate.ExposureHidden)
	}
}

// TestAdminViewerExposure_Rejects covers the two 400 paths: an unknown
// catalog key and an exposure value outside the known set.
func TestAdminViewerExposure_Rejects(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	cases := []map[string]any{
		{"feature_key": "does_not_exist", "exposure": featuregate.ExposureHidden},
		{"feature_key": featuregate.KeyClockLayout, "exposure": "bananas"},
	}
	for _, body := range cases {
		resp := postAdminViewerJSON(t, env, "/a/viewers/"+testViewerMAC+"/exposure", body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %v -> status %d, want 400", body, resp.StatusCode)
		}
	}
}

// TestAdminViewerTemplate_AssignAndClear assigns a template, then clears it,
// verifying viewers.template_id through the store getter both ways.
func TestAdminViewerTemplate_AssignAndClear(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	ctx := context.Background()
	st := featuregate.NewStore(env.d.DB)
	tmplID, err := st.CreateTemplate(ctx, "Standard")
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}

	// Assign.
	resp := postAdminViewerJSON(t, env, "/a/viewers/"+testViewerMAC+"/template", map[string]any{
		"template_id": tmplID,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("assign status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	id, name, found, err := st.ViewerTemplate(ctx, testViewerMAC)
	if err != nil {
		t.Fatalf("ViewerTemplate: %v", err)
	}
	if !found || id != tmplID || name != "Standard" {
		t.Errorf("after assign: found=%v id=%d name=%q, want true %d Standard", found, id, name, tmplID)
	}

	// Clear (template_id 0).
	resp = postAdminViewerJSON(t, env, "/a/viewers/"+testViewerMAC+"/template", map[string]any{
		"template_id": 0,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	if _, _, found, _ = st.ViewerTemplate(ctx, testViewerMAC); found {
		t.Errorf("after clear: template still assigned")
	}
}

// TestAdminViewerTemplate_RejectsUnknown proves a non-existent template id is a
// clean 400 (not an FK 500).
func TestAdminViewerTemplate_RejectsUnknown(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	resp := postAdminViewerJSON(t, env, "/a/viewers/"+testViewerMAC+"/template", map[string]any{
		"template_id": 999999,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for unknown template", resp.StatusCode)
	}
}

// TestAdminViewerDetail_FunctionListMarkup verifies the page renders the
// Saison-20 function list (three-level exposure selector per function) and the
// Vorlage/Abo frames, and that the old binary visibility markup is gone.
func TestAdminViewerDetail_FunctionListMarkup(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	adoptESPForTest(t, env, espTestMAC, "Wohnung ESP Feat")

	resp, err := env.client.Get(env.ts.URL + "/a/viewers/" + espTestMAC)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	markup := detailPageMarkup(readBody(t, resp))

	// S20 card grid: per-function cells with the config-mode master switch and
	// the three-state visibility control (X/check/lock = admin_only/
	// tenant_visible/bookable; "hidden" is the active switch off).
	for _, want := range []string{
		`id="vd-grid"`,
		`data-vd-config`, // master switch "Aktivierung verwalten"
		`data-feature-key="keep_stream_in_screensaver"`,
		`name="vis_keep_stream_in_screensaver"`,
		`name="vis_idle_view_mode"`,
		`value="admin_only"`,
		`value="tenant_visible"`,
		`value="bookable"`,
		`id="template-select"`,
		"Abo",
	} {
		if !contains(markup, want) {
			t.Errorf("card-grid markup missing %q", want)
		}
	}
	// The old binary visibility control is gone.
	if contains(markup, `data-vis-key`) {
		t.Errorf("legacy binary visibility markup (data-vis-key) still present")
	}
}

// TestAdminViewerDetail_AndroidTokenModal locks the redesign fix: the
// "Token neu generieren" button renders for Android, so its modals must
// render too (they were ESP-only before, leaving the button dead). The
// password modal must NOT render on Android (no reset-password button).
func TestAdminViewerDetail_AndroidTokenModal(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	const androidMAC = "0c:ea:14:77:77:77"
	seedAndroidViewer(t, env, androidMAC, 8203)

	resp, err := env.client.Get(env.ts.URL + "/a/viewers/" + androidMAC)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	markup := detailPageMarkup(readBody(t, resp))

	if !contains(markup, `data-action="regen-token"`) {
		t.Errorf("Android regen-token button missing")
	}
	if !contains(markup, `id="token-confirm-modal"`) || !contains(markup, `id="token-display-modal"`) {
		t.Errorf("Android token modals missing -> regen button would be a dead control")
	}
	if contains(markup, `id="password-modal"`) {
		t.Errorf("password modal should not render on Android")
	}
}

// TestAdminViewerDetail_AboFrame proves a seeded license renders in the Abo
// frame (plan name + viewer limit).
func TestAdminViewerDetail_AboFrame(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)

	limit := 10
	st := featuregate.NewStore(env.d.DB)
	if err := st.SetLicense(context.Background(), "Pro", &limit, nil); err != nil {
		t.Fatalf("SetLicense: %v", err)
	}

	resp, err := env.client.Get(env.ts.URL + "/a/viewers/" + testViewerMAC)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	markup := detailPageMarkup(readBody(t, resp))

	if !contains(markup, "Pro") {
		t.Errorf("Abo frame missing plan name 'Pro'")
	}
	if !contains(markup, "/ 10") {
		t.Errorf("Abo frame missing viewer limit '/ 10'")
	}
}
