package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"carvilon.local/server/internal/featuregate"
)

// A template attached to the viewer overrides the (unset) keep_stream value,
// and the additive gating block is delivered. The OTHER keep_stream flag, which
// the template does not touch, keeps the plain web default (true). Rollout 2a:
// the flat fields stay present; gating is purely additive.
func TestMieterSettingsJSON_TemplateOverridesValue_AndGating(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env) // seeds + signs in testViewerMAC (type web)

	st := featuregate.NewStore(env.d.DB)
	ctx := context.Background()
	id, err := st.CreateTemplate(ctx, "Sparmodus")
	if err != nil {
		t.Fatalf("create template: %v", err)
	}
	// Screensaver flag forced false via the template; screen-off untouched.
	val := "false"
	if err := st.SetTemplateFeature(ctx, id, featuregate.KeyKeepStreamInScreensaver, nil, &val); err != nil {
		t.Fatalf("set template feature: %v", err)
	}
	if err := st.AssignViewerTemplate(ctx, testViewerMAC, &id); err != nil {
		t.Fatalf("assign template: %v", err)
	}

	body := getSettingsJSON(t, env)

	// Overridden by the template (web default would be true).
	if body["keep_stream_in_screensaver"] != false {
		t.Errorf("keep_stream_in_screensaver = %v, want false (template override)", body["keep_stream_in_screensaver"])
	}
	// Untouched -> plain web default true; still present (2a, no field dropped).
	if body["keep_stream_in_screen_off"] != true {
		t.Errorf("keep_stream_in_screen_off = %v, want true (web default, untouched)", body["keep_stream_in_screen_off"])
	}

	// Additive gating block.
	gating, ok := body["gating"].(map[string]any)
	if !ok {
		t.Fatalf("gating block missing or wrong type: %T", body["gating"])
	}
	scr, ok := gating[featuregate.KeyKeepStreamInScreensaver].(map[string]any)
	if !ok {
		t.Fatalf("gating[%s] missing", featuregate.KeyKeepStreamInScreensaver)
	}
	if scr["licensed"] != true || scr["active"] != true {
		t.Errorf("gating screensaver = %v, want {licensed:true active:true}", scr)
	}
}

// License lock (rollout 2a): an unlicensed function is flagged licensed:false in
// the gating block, but its flat value is STILL delivered unchanged - no field
// weglassen in this step.
func TestMieterSettingsJSON_LicenseLock_KeepsValue_FlagsGating(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env)

	st := featuregate.NewStore(env.d.DB)
	if err := st.SetLicenseFeature(context.Background(), featuregate.KeyKeepStreamInScreensaver, false); err != nil {
		t.Fatalf("set license feature: %v", err)
	}

	body := getSettingsJSON(t, env)

	// 2a: value NOT dropped, still the web default true.
	if _, present := body["keep_stream_in_screensaver"]; !present {
		t.Errorf("keep_stream_in_screensaver must stay present (rollout 2a)")
	}
	if body["keep_stream_in_screensaver"] != true {
		t.Errorf("keep_stream_in_screensaver = %v, want true (value still delivered)", body["keep_stream_in_screensaver"])
	}
	gating, ok := body["gating"].(map[string]any)
	if !ok {
		t.Fatalf("gating block missing")
	}
	scr := gating[featuregate.KeyKeepStreamInScreensaver].(map[string]any)
	if scr["licensed"] != false {
		t.Errorf("gating screensaver licensed = %v, want false (locked)", scr["licensed"])
	}
}

// broadcastTemplateChanged fans config.changed out over the per-MAC eventbus to
// every viewer attached to the template, and to nobody else.
func TestBroadcastTemplateChanged_FansOutPerMAC(t *testing.T) {
	env := newTestServer(t)
	const attachedMAC = testViewerMAC
	const otherMAC = "0c:ea:14:42:42:43"
	env.seedViewer(t) // attachedMAC, type web
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

	// Attached viewer receives config.changed.
	select {
	case ev := <-attachedCh:
		if ev.Type != "config.changed" {
			t.Errorf("attached event type = %q, want config.changed", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatalf("attached viewer did not receive config.changed")
	}

	// Unattached viewer receives nothing.
	select {
	case ev := <-otherCh:
		t.Errorf("unattached viewer received an event: %+v", ev)
	case <-time.After(200 * time.Millisecond):
		// expected: no event
	}
}

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
