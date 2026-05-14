package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"unifix.local/server/internal/platformconfig"
)

func postIntercomMapping(t *testing.T, env *testEnv, body any) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/a/intercom-mapping", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return resp
}

// Saison 13-06 happy path: new body shape with both maps lands
// in the right platformconfig keys.
func TestIntercomMappingPost_PersistsBothMaps(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	resp := postIntercomMapping(t, env, map[string]any{
		"intercom_mapping": map[string]string{
			"28:70:4e:31:e2:9c": "door-uuid-front",
		},
		"viewer_mapping": map[string]string{
			"0c:ea:14:79:95:75": "door-uuid-front",
			"0c:ea:14:0a:78:06": "door-uuid-side",
		},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["intercom_count"] != float64(1) {
		t.Errorf("intercom_count = %v, want 1", got["intercom_count"])
	}
	if got["viewer_count"] != float64(2) {
		t.Errorf("viewer_count = %v, want 2", got["viewer_count"])
	}

	intercom, _ := env.platformCfg.IntercomToDoor(context.Background())
	if intercom["28:70:4e:31:e2:9c"] != "door-uuid-front" {
		t.Errorf("intercom map = %+v", intercom)
	}
	viewer, _ := env.platformCfg.ViewerToDoor(context.Background())
	if viewer["0c:ea:14:79:95:75"] != "door-uuid-front" {
		t.Errorf("viewer map = %+v", viewer)
	}
	if viewer["0c:ea:14:0a:78:06"] != "door-uuid-side" {
		t.Errorf("viewer map = %+v", viewer)
	}
}

// Saison 13-05 back-compat: the legacy {"mapping": {...}} body
// still works and only touches the intercom side.
func TestIntercomMappingPost_LegacyMappingKeyOnlyTouchesIntercom(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	// Pre-existing viewer mapping must NOT be wiped by a legacy
	// POST that omits the viewer_mapping key.
	if err := env.platformCfg.SetViewerToDoor(context.Background(),
		map[string]string{"0c:ea:14:79:95:75": "door-uuid-survives"}); err != nil {
		t.Fatalf("SetViewerToDoor: %v", err)
	}

	resp := postIntercomMapping(t, env, map[string]any{
		"mapping": map[string]string{
			"28:70:4e:31:e2:9c": "door-uuid-front",
		},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	intercom, _ := env.platformCfg.IntercomToDoor(context.Background())
	if intercom["28:70:4e:31:e2:9c"] != "door-uuid-front" {
		t.Errorf("intercom map = %+v", intercom)
	}
	viewer, _ := env.platformCfg.ViewerToDoor(context.Background())
	if viewer["0c:ea:14:79:95:75"] != "door-uuid-survives" {
		t.Errorf("legacy POST wiped viewer mapping: got %+v", viewer)
	}
}

// Saison 13-06 partial save: omitting viewer_mapping leaves the
// stored value untouched. Empty {} would clear it; missing key is
// "don't touch".
func TestIntercomMappingPost_OmittedSectionPreserved(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	if err := env.platformCfg.SetViewerToDoor(context.Background(),
		map[string]string{"0c:ea:14:aa:aa:aa": "door-old"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Send only intercom_mapping; viewer_mapping is missing
	// from the JSON entirely.
	resp := postIntercomMapping(t, env, map[string]any{
		"intercom_mapping": map[string]string{},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	viewer, _ := env.platformCfg.ViewerToDoor(context.Background())
	if viewer["0c:ea:14:aa:aa:aa"] != "door-old" {
		t.Errorf("partial save wiped viewer mapping: %+v", viewer)
	}
}

// Saison 13-06 explicit clear: empty viewer_mapping wipes the
// stored value (so the operator can clear all viewer assignments
// from the page).
func TestIntercomMappingPost_EmptyViewerMappingClears(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	_ = env.platformCfg.SetViewerToDoor(context.Background(),
		map[string]string{"0c:ea:14:bb:bb:bb": "door-bye"})

	resp := postIntercomMapping(t, env, map[string]any{
		"viewer_mapping": map[string]string{},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	viewer, _ := env.platformCfg.ViewerToDoor(context.Background())
	if len(viewer) != 0 {
		t.Errorf("explicit empty mapping did not clear: %+v", viewer)
	}
}

// Force a reference to platformconfig at package level so the
// import survives if helpers move.
var _ = platformconfig.KeyViewerToDoor
