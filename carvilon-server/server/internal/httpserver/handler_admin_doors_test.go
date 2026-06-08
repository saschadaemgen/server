package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"carvilon.local/server/internal/uaapi"
	"carvilon.local/server/internal/viewermanager"
)

// TestAdminDoorsJSON_ListsDoors proves /a/doors.json returns the live
// UA-API door list (door_id + name) - the source for the per-viewer
// door-assignment UI (S19-30 Teil B).
func TestAdminDoorsJSON_ListsDoors(t *testing.T) {
	uaStub := newUADoorsStub(t, uaDoorStubConfig{
		doors: map[string]string{"door-uuid-front": "aabbccddeeff"},
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

// Saison 19-32 one-step flow: POST/DELETE /a/viewers/{mac}/doors/{door_id}
// persist a single door immediately.
func TestAdminViewerAddDoor_PersistsImmediately(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env)

	url := env.ts.URL + "/a/viewers/" + testViewerMAC + "/doors/door-uuid-front?label=Haupteingang"
	req, _ := http.NewRequest(http.MethodPost, url, nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST add door: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got, _ := env.viewerMgr.ListViewerDoors(context.Background(), testViewerMAC)
	if len(got) != 1 || got[0].DoorID != "door-uuid-front" || got[0].Label != "Haupteingang" {
		t.Fatalf("after add = %+v, want one front/Haupteingang", got)
	}

	// Idempotent: adding the same door again stays at one.
	req2, _ := http.NewRequest(http.MethodPost, url, nil)
	r2, _ := env.client.Do(req2)
	r2.Body.Close()
	got2, _ := env.viewerMgr.ListViewerDoors(context.Background(), testViewerMAC)
	if len(got2) != 1 {
		t.Errorf("after re-add = %d, want 1 (idempotent)", len(got2))
	}
}

func TestAdminViewerRemoveDoor_PersistsImmediately(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env)
	if err := env.viewerMgr.AddViewerDoor(context.Background(), testViewerMAC,
		viewermanager.DoorAssignment{DoorID: "door-uuid-front"}); err != nil {
		t.Fatalf("AddViewerDoor: %v", err)
	}

	req, _ := http.NewRequest(http.MethodDelete,
		env.ts.URL+"/a/viewers/"+testViewerMAC+"/doors/door-uuid-front", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("DELETE door: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got, _ := env.viewerMgr.ListViewerDoors(context.Background(), testViewerMAC)
	if len(got) != 0 {
		t.Errorf("after remove = %d, want 0", len(got))
	}
}

// The empty-replace-all guard: a {doors:[]} POST must be rejected and
// must NOT wipe an existing assignment (the S19-31 bug).
func TestAdminViewerDoors_EmptyReplaceAllRejected(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env)
	if err := env.viewerMgr.AddViewerDoor(context.Background(), testViewerMAC,
		viewermanager.DoorAssignment{DoorID: "door-uuid-front"}); err != nil {
		t.Fatalf("AddViewerDoor: %v", err)
	}

	resp := postAdminViewerJSON(t, env, "/a/viewers/"+testViewerMAC+"/doors",
		map[string]any{"doors": []any{}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty replace-all status = %d, want 400 (guard)", resp.StatusCode)
	}
	got, _ := env.viewerMgr.ListViewerDoors(context.Background(), testViewerMAC)
	if len(got) != 1 {
		t.Errorf("assignment after rejected empty save = %d, want 1 (must NOT be wiped)", len(got))
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
