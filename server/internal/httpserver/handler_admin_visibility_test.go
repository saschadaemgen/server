package httpserver

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestAdminViewerVisibility_PersistsAndSurfacesInSettingsJSON: the admin
// hides a setting via POST /a/viewers/{mac}/visibility, and the tenant
// settings.json then carries visibility[key]=false while the flat fields
// stay present (S19-39).
func TestAdminViewerVisibility_PersistsAndSurfacesInSettingsJSON(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env) // seeds + signs in testViewerMAC

	resp := postAdminViewerJSON(t, env, "/a/viewers/"+testViewerMAC+"/visibility",
		map[string]any{"setting_key": "language", "visible": false})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST visibility status = %d, want 200", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/webviewer/settings.json", nil)
	r2, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("GET settings.json: %v", err)
	}
	defer r2.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(r2.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	vis, ok := body["visibility"].(map[string]any)
	if !ok {
		t.Fatalf("settings.json has no visibility map: %v", body)
	}
	if vis["language"] != false {
		t.Errorf("visibility.language = %v, want false", vis["language"])
	}
	// Flat fields must still be present (the S19-37 contract is byte-shape stable).
	for _, k := range []string{"idle_view_mode", "language", "clock_layout", "path_mode", "unit_name"} {
		if _, present := body[k]; !present {
			t.Errorf("flat field %q missing after a visibility change", k)
		}
	}
}

// No visibility overrides -> the map is omitted entirely (flat contract
// stays byte-identical for unconfigured viewers).
func TestMieterSettingsJSON_NoVisibilityOmitsMap(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env)

	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/webviewer/settings.json", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := body["visibility"]; present {
		t.Errorf("visibility key present with no overrides; want omitted (omitempty)")
	}
}
