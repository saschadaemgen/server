package httpserver

import (
	"context"
	"net/http"
	"testing"

	"carvilon.local/server/internal/uaapi"
	"carvilon.local/server/internal/viewermanager"
)

// Saison 19-30 unlock-by-door + authorisation tests. The UA stub
// captures the door UUID actually sent to PUT .../unlock so each
// test can assert both the HTTP status AND that an unauthorised /
// ambiguous request opens NOTHING.

func unlockEnv(t *testing.T, gotDoorID *string) *testEnv {
	t.Helper()
	uaStub := newUADoorsStub(t, uaDoorStubConfig{doors: map[string]string{}}, gotDoorID)
	t.Cleanup(uaStub.Close)
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env)
	env.srv.SetUAClient(uaapi.New(uaapi.Options{BaseURL: uaStub.URL, Token: "t"}))
	return env
}

func postUnlock(t *testing.T, env *testEnv, doorParam string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/webviewer/doors/"+doorParam+"/unlock", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST unlock: %v", err)
	}
	return resp
}

func assignDoors(t *testing.T, env *testEnv, ids ...string) {
	t.Helper()
	doors := make([]viewermanager.DoorAssignment, 0, len(ids))
	for i, id := range ids {
		doors = append(doors, viewermanager.DoorAssignment{DoorID: id, Sort: i})
	}
	if err := env.viewerMgr.SetViewerDoors(context.Background(), testViewerMAC, doors); err != nil {
		t.Fatalf("SetViewerDoors: %v", err)
	}
}

// Direct UUID + assigned -> 200, opens exactly that door (no intercom hop).
func TestMieterUnlock_DirectAssignedDoorOpens(t *testing.T) {
	var gotDoorID string
	env := unlockEnv(t, &gotDoorID)
	assignDoors(t, env, "door-uuid-front")

	resp := postUnlock(t, env, "door-uuid-front")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", resp.StatusCode, readBody(t, resp))
	}
	if gotDoorID != "door-uuid-front" {
		t.Errorf("UA-API opened %q, want door-uuid-front (direct)", gotDoorID)
	}
}

// Direct UUID + NOT assigned -> 403, opens nothing (the authz gate).
func TestMieterUnlock_DirectUnassignedDoorDenied403(t *testing.T) {
	var gotDoorID string
	env := unlockEnv(t, &gotDoorID)
	assignDoors(t, env, "door-uuid-front")

	resp := postUnlock(t, env, "door-uuid-stranger")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (door not assigned)", resp.StatusCode)
	}
	if gotDoorID != "" {
		t.Errorf("UA-API was called with %q; an unassigned door must NEVER open", gotDoorID)
	}
}

// standby + exactly one assigned door -> opens it directly.
func TestMieterUnlock_StandbyOneAssignedDoorOpens(t *testing.T) {
	var gotDoorID string
	env := unlockEnv(t, &gotDoorID)
	assignDoors(t, env, "door-uuid-only")

	resp := postUnlock(t, env, "standby")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", resp.StatusCode, readBody(t, resp))
	}
	if gotDoorID != "door-uuid-only" {
		t.Errorf("standby opened %q, want the single assigned door", gotDoorID)
	}
}

// standby + multiple assigned doors -> 409, opens nothing (client must pick).
func TestMieterUnlock_StandbyMultipleAssignedReturns409(t *testing.T) {
	var gotDoorID string
	env := unlockEnv(t, &gotDoorID)
	assignDoors(t, env, "door-a", "door-b")

	resp := postUnlock(t, env, "standby")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409 (ambiguous standby)", resp.StatusCode)
	}
	if gotDoorID != "" {
		t.Errorf("UA-API called with %q; ambiguous standby must open nothing", gotDoorID)
	}
}
