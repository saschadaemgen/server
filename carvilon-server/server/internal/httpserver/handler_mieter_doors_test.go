package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"carvilon.local/server/internal/uaapi"
	"carvilon.local/server/internal/viewermanager"
)

// TestMieterDoors_ReturnsAssignedWithNames proves GET /webviewer/doors
// returns the authenticated viewer's assigned doors, with the name
// resolved from the live UA list when present and the admin label
// otherwise (S19-30 Teil E).
func TestMieterDoors_ReturnsAssignedWithNames(t *testing.T) {
	uaStub := newUADoorsStub(t, uaDoorStubConfig{
		doors: map[string]string{"door-uuid-front": "aabbccddeeff"},
	}, nil)
	defer uaStub.Close()

	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env)
	env.srv.SetUAClient(uaapi.New(uaapi.Options{BaseURL: uaStub.URL, Token: "t"}))

	if err := env.viewerMgr.SetViewerDoors(context.Background(), testViewerMAC,
		[]viewermanager.DoorAssignment{
			{DoorID: "door-uuid-front", Sort: 0},          // name from UA list
			{DoorID: "door-uuid-custom", Label: "Garage"}, // name from admin label
		}); err != nil {
		t.Fatalf("SetViewerDoors: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/webviewer/doors", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("GET /webviewer/doors: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Doors []struct {
			DoorID string `json:"door_id"`
			Name   string `json:"name"`
			Label  string `json:"label"`
		} `json:"doors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Doors) != 2 {
		t.Fatalf("doors = %d, want 2", len(body.Doors))
	}
	if body.Doors[0].DoorID != "door-uuid-front" || body.Doors[0].Name != "Door door-uuid-front" {
		t.Errorf("door[0] = %+v, want front with UA name", body.Doors[0])
	}
	if body.Doors[1].Name != "Garage" {
		t.Errorf("door[1].Name = %q, want label fallback 'Garage'", body.Doors[1].Name)
	}
}

// TestMieterDoors_RequiresViewerAuth: no viewer session -> not 200.
func TestMieterDoors_RequiresViewerAuth(t *testing.T) {
	env := newTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/webviewer/doors", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("status = 200 without viewer auth, want 401/redirect")
	}
}
