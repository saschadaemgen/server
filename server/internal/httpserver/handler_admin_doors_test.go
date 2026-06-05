package httpserver

import (
	"context"
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

// TestAdminViewerDoors_SaveRoundTrip proves POST /a/viewers/{mac}/doors
// persists the 1:n assignment (replace-all) for any viewer (S19-30 Teil D).
func TestAdminViewerDoors_SaveRoundTrip(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env) // seeds testViewerMAC

	body := map[string]any{"doors": []map[string]any{
		{"door_id": "door-uuid-front", "label": "Haupteingang"},
		{"door_id": "door-uuid-back"},
	}}
	resp := postAdminViewerJSON(t, env, "/a/viewers/"+testViewerMAC+"/doors", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST doors status = %d, want 200", resp.StatusCode)
	}

	got, err := env.viewerMgr.ListViewerDoors(context.Background(), testViewerMAC)
	if err != nil {
		t.Fatalf("ListViewerDoors: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("persisted doors = %d, want 2", len(got))
	}
	if got[0].DoorID != "door-uuid-front" || got[0].Label != "Haupteingang" {
		t.Errorf("door[0] = %+v, want front/Haupteingang", got[0])
	}

	// Replace-all: posting a shorter list clears the rest.
	resp2 := postAdminViewerJSON(t, env, "/a/viewers/"+testViewerMAC+"/doors",
		map[string]any{"doors": []map[string]any{{"door_id": "door-uuid-front"}}})
	resp2.Body.Close()
	got2, _ := env.viewerMgr.ListViewerDoors(context.Background(), testViewerMAC)
	if len(got2) != 1 || got2[0].DoorID != "door-uuid-front" {
		t.Errorf("after replace = %+v, want [front]", got2)
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
