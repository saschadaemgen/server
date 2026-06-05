package httpserver

import (
	"encoding/json"
	"net/http"
	"testing"

	"carvilon.local/server/internal/uaapi"
)

// TestAdminDoorsJSON_ListsDoors proves /a/doors.json returns the live
// UA-API door list (door_id + name) - the source for the per-viewer
// door-assignment UI (S19-30 Teil B).
func TestAdminDoorsJSON_ListsDoors(t *testing.T) {
	uaStub := newUADoorsStub(t, uaDoorStubConfig{
		doors: map[string]string{"door-uuid-front": "28704e31e29c"},
	}, nil)
	defer uaStub.Close()

	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.srv.SetUAClient(uaapi.New(uaapi.Options{BaseURL: uaStub.URL, Token: "t"}))

	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/a/doors.json", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("GET /a/doors.json: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Configured bool `json:"configured"`
		Doors      []struct {
			DoorID string `json:"door_id"`
			Name   string `json:"name"`
		} `json:"doors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Configured {
		t.Error("configured = false, want true")
	}
	if len(body.Doors) != 1 || body.Doors[0].DoorID != "door-uuid-front" {
		t.Fatalf("doors = %+v, want one door-uuid-front", body.Doors)
	}
	if body.Doors[0].Name == "" {
		t.Error("door name is empty, want the UA-reported name")
	}
}

// TestAdminDoorsJSON_RequiresAdmin: no admin session -> not 200.
func TestAdminDoorsJSON_RequiresAdmin(t *testing.T) {
	env := newTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/a/doors.json", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("status = 200 without admin session, want redirect/401")
	}
}
