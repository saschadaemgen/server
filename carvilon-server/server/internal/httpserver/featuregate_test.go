package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"carvilon.local/server/internal/featuregate"
)

// A template attached to the viewer overrides the (unset) keep_stream value and
// its exposure, and the additive gating block carries {licensed, exposure,
// writable}. The untouched flag keeps the plain web default and stays present.
func TestMieterSettingsJSON_TemplateExposureAndGating(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env) // seeds + signs in testViewerMAC (type web)

	st := featuregate.NewStore(env.d.DB)
	ctx := context.Background()
	id, err := st.CreateTemplate(ctx, "Sparmodus")
	if err != nil {
		t.Fatalf("create template: %v", err)
	}
	// keep_stream_in_screensaver forced false via the template; screen-off untouched.
	if err := st.SetTemplateFeature(ctx, id, featuregate.KeyKeepStreamInScreensaver, nil, ptr("false")); err != nil {
		t.Fatalf("set template feature: %v", err)
	}
	if err := st.AssignViewerTemplate(ctx, testViewerMAC, &id); err != nil {
		t.Fatalf("assign template: %v", err)
	}

	body := getSettingsJSON(t, env)
	if body["keep_stream_in_screensaver"] != false {
		t.Errorf("keep_stream_in_screensaver = %v, want false (template override)", body["keep_stream_in_screensaver"])
	}
	if body["keep_stream_in_screen_off"] != true {
		t.Errorf("keep_stream_in_screen_off = %v, want true (web default, untouched)", body["keep_stream_in_screen_off"])
	}
	gating, ok := body["gating"].(map[string]any)
	if !ok {
		t.Fatalf("gating block missing or wrong type: %T", body["gating"])
	}
	scr, ok := gating[featuregate.KeyKeepStreamInScreensaver].(map[string]any)
	if !ok {
		t.Fatalf("gating[%s] missing", featuregate.KeyKeepStreamInScreensaver)
	}
	if scr["licensed"] != true || scr["exposure"] != "tenant_visible" || scr["writable"] != true {
		t.Errorf("gating screensaver = %v, want {licensed:true exposure:tenant_visible writable:true}", scr)
	}
}

// License lock (rollout 2a): unlicensed -> writable false in gating, but the
// flat value is STILL delivered unchanged.
func TestMieterSettingsJSON_LicenseLock_KeepsValue(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env)

	st := featuregate.NewStore(env.d.DB)
	if err := st.SetLicenseFeature(context.Background(), featuregate.KeyKeepStreamInScreensaver, false); err != nil {
		t.Fatalf("set license feature: %v", err)
	}

	body := getSettingsJSON(t, env)
	if _, present := body["keep_stream_in_screensaver"]; !present {
		t.Errorf("keep_stream_in_screensaver must stay present (rollout 2a)")
	}
	if body["keep_stream_in_screensaver"] != true {
		t.Errorf("keep_stream_in_screensaver = %v, want true (value still delivered)", body["keep_stream_in_screensaver"])
	}
	gating := body["gating"].(map[string]any)
	scr := gating[featuregate.KeyKeepStreamInScreensaver].(map[string]any)
	if scr["licensed"] != false || scr["writable"] != false {
		t.Errorf("gating screensaver = %v, want licensed:false writable:false (locked)", scr)
	}
}

// Derived visibility block (web-compat): an admin_only exposure on a legacy key
// surfaces as visible=false; default tenant_visible -> visible=true.
func TestMieterSettingsJSON_DerivedVisibility(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env)

	st := featuregate.NewStore(env.d.DB)
	ctx := context.Background()
	if err := st.SetViewerExposure(ctx, testViewerMAC, "language", featuregate.ExposureAdminOnly); err != nil {
		t.Fatalf("set exposure language: %v", err)
	}
	if err := st.SetViewerExposure(ctx, testViewerMAC, "clock_layout", featuregate.ExposureTenantVisible); err != nil {
		t.Fatalf("set exposure clock_layout: %v", err)
	}

	body := getSettingsJSON(t, env)
	vis, ok := body["visibility"].(map[string]any)
	if !ok {
		t.Fatalf("visibility block missing: %T", body["visibility"])
	}
	if vis["language"] != false {
		t.Errorf("visibility[language] = %v, want false (admin_only)", vis["language"])
	}
	if vis["clock_layout"] != true {
		t.Errorf("visibility[clock_layout] = %v, want true (tenant_visible)", vis["clock_layout"])
	}
}

// Write-back accepts a tenant_visible, write-capable field, persists it, and
// fans config.changed to the tenant (today = the writer's own MAC).
func TestWriteBack_AcceptsTenantVisible_AndBroadcasts(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env)

	bus := env.srv.EventBus()
	ch := bus.Subscribe(testViewerMAC)
	defer bus.Unsubscribe(testViewerMAC, ch)

	postWriteBack(t, env, map[string]any{"keep_stream_in_screensaver": false}, http.StatusOK)

	// Value persisted (web default is true; write flipped it to false).
	body := getSettingsJSON(t, env)
	if body["keep_stream_in_screensaver"] != false {
		t.Errorf("after write-back keep_stream_in_screensaver = %v, want false", body["keep_stream_in_screensaver"])
	}
	// config.changed fanned to the tenant (the writer's own MAC, here).
	select {
	case ev := <-ch:
		if ev.Type != "config.changed" {
			t.Errorf("event type = %q, want config.changed", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatalf("write-back did not fan config.changed to the tenant")
	}
}

// Write-back rejects a field whose exposure is admin_only (not tenant-writable);
// nothing is written.
func TestWriteBack_RejectsAdminOnly(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env)

	st := featuregate.NewStore(env.d.DB)
	if err := st.SetViewerExposure(context.Background(), testViewerMAC, featuregate.KeyKeepStreamInScreensaver, featuregate.ExposureAdminOnly); err != nil {
		t.Fatalf("set exposure: %v", err)
	}
	postWriteBack(t, env, map[string]any{"keep_stream_in_screensaver": false}, http.StatusForbidden)
}

// Write-back rejects an unknown field key.
func TestWriteBack_RejectsUnknownField(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env)
	postWriteBack(t, env, map[string]any{"not_a_real_field": true}, http.StatusBadRequest)
}

// broadcastTemplateChanged fans config.changed to every viewer attached to the
// template, and to nobody else.
func TestBroadcastTemplateChanged_FansOutPerMAC(t *testing.T) {
	env := newTestServer(t)
	const attachedMAC = testViewerMAC
	const otherMAC = "0c:ea:14:42:42:43"
	env.seedViewer(t)
	env.seedViewerAs(t, otherMAC, "Other Unit", testViewerPassword)

	st := featuregate.NewStore(env.d.DB)
	ctx := context.Background()
	id, err := st.CreateTemplate(ctx, "T")
	if err != nil {
		t.Fatalf("create template: %v", err)
	}
	if err := st.AssignViewerTemplate(ctx, attachedMAC, &id); err != nil {
		t.Fatalf("assign template: %v", err)
	}

	bus := env.srv.EventBus()
	attachedCh := bus.Subscribe(attachedMAC)
	defer bus.Unsubscribe(attachedMAC, attachedCh)
	otherCh := bus.Subscribe(otherMAC)
	defer bus.Unsubscribe(otherMAC, otherCh)

	env.srv.broadcastTemplateChanged(ctx, id)

	select {
	case ev := <-attachedCh:
		if ev.Type != "config.changed" {
			t.Errorf("attached event type = %q, want config.changed", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatalf("attached viewer did not receive config.changed")
	}
	select {
	case ev := <-otherCh:
		t.Errorf("unattached viewer received an event: %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}
}

func ptr(s string) *string { return &s }

func getSettingsJSON(t *testing.T, env *testEnv) map[string]any {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/webviewer/settings.json", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("GET /webviewer/settings.json: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

func postWriteBack(t *testing.T, env *testEnv, fields map[string]any, wantStatus int) {
	t.Helper()
	buf, _ := json.Marshal(fields)
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/webviewer/settings", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST write-back: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("write-back status = %d, want %d (body=%s)", resp.StatusCode, wantStatus, readBody(t, resp))
	}
}
