package httpserver

import (
	"context"
	"net/http"
	"testing"

	"carvilon.local/server/internal/viewermanager"
)

// TestAdminViewerDetail_AndroidShowsTokenNotPassword proves the
// shared viewer-detail page renders correctly for type=android
// (S19-30 Teil C): the bearer-token section (not the web password
// section, not the ESP-hardware settings) and the android back-link.
func TestAdminViewerDetail_AndroidShowsTokenNotPassword(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	const androidMAC = "0c:ea:14:77:77:77"
	if err := env.viewerMgr.AddViewer(context.Background(), viewermanager.ViewerSpec{
		MAC:         androidMAC,
		Name:        "Wohnung Android Detail",
		ServicePort: 8190,
		Type:        viewermanager.TypeAndroid,
	}); err != nil {
		t.Fatalf("AddViewer android: %v", err)
	}

	resp, err := env.client.Get(env.ts.URL + "/a/viewers/" + androidMAC)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	markup := detailPageMarkup(readBody(t, resp))

	if !contains(markup, `data-action="regen-token"`) {
		t.Errorf("Token-Regen-Button fehlt (Android-Viewer)")
	}
	if contains(markup, `data-action="reset-password"`) {
		t.Errorf("Password-Button auf Android sichtbar (sollte nur Web sein)")
	}
	if contains(markup, `name="brightness_idle"`) {
		t.Errorf("ESP-Hardware-Settings auf Android sichtbar (sollten nur ESP sein)")
	}
	if !contains(markup, "/a/android-viewers") {
		t.Errorf("Android-Back-Link fehlt")
	}
}
