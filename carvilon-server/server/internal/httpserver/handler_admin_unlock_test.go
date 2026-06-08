package httpserver

import (
	"context"
	"net/http"
	"testing"

	"carvilon.local/server/internal/uaapi"
	"carvilon.local/server/internal/viewermanager"
)

// adminUnlockEnv: admin session + a seeded web viewer (testViewerMAC)
// + a UA stub that captures the door UUID actually opened. doors maps
// door-uuid -> intercom-bare for the paired-fallback path.
func adminUnlockEnv(t *testing.T, doors map[string]string, gotDoorID *string) *testEnv {
	t.Helper()
	uaStub := newUADoorsStub(t, uaDoorStubConfig{doors: doors}, gotDoorID)
	t.Cleanup(uaStub.Close)
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.seedViewer(t)
	env.srv.SetUAClient(uaapi.New(uaapi.Options{BaseURL: uaStub.URL, Token: "t"}))
	return env
}

func adminUnlock(t *testing.T, env *testEnv, query string) *http.Response {
	t.Helper()
	url := env.ts.URL + "/a/viewers/" + testViewerMAC + "/unlock"
	if query != "" {
		url += "?" + query
	}
	req, _ := http.NewRequest(http.MethodPost, url, nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST admin unlock: %v", err)
	}
	return resp
}

// Exactly one assigned door -> opens it.
func TestAdminUnlock_OneAssignedDoorOpens(t *testing.T) {
	var got string
	env := adminUnlockEnv(t, nil, &got)
	if err := env.viewerMgr.SetViewerDoors(context.Background(), testViewerMAC,
		[]viewermanager.DoorAssignment{{DoorID: "door-only"}}); err != nil {
		t.Fatalf("SetViewerDoors: %v", err)
	}
	resp := adminUnlock(t, env, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", resp.StatusCode, readBody(t, resp))
	}
	if got != "door-only" {
		t.Errorf("opened %q, want door-only", got)
	}
}

// Several assigned doors and no door_id -> 409, opens nothing.
func TestAdminUnlock_MultipleDoorsNeedsPick409(t *testing.T) {
	var got string
	env := adminUnlockEnv(t, nil, &got)
	if err := env.viewerMgr.SetViewerDoors(context.Background(), testViewerMAC,
		[]viewermanager.DoorAssignment{{DoorID: "door-a"}, {DoorID: "door-b"}}); err != nil {
		t.Fatalf("SetViewerDoors: %v", err)
	}
	resp := adminUnlock(t, env, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409 (multiple doors)", resp.StatusCode)
	}
	if got != "" {
		t.Errorf("opened %q on ambiguous admin unlock, want nothing", got)
	}
}

// Several doors + explicit door_id -> opens that one (admin-trusted).
func TestAdminUnlock_ExplicitDoorIDOpens(t *testing.T) {
	var got string
	env := adminUnlockEnv(t, nil, &got)
	if err := env.viewerMgr.SetViewerDoors(context.Background(), testViewerMAC,
		[]viewermanager.DoorAssignment{{DoorID: "door-a"}, {DoorID: "door-b"}}); err != nil {
		t.Fatalf("SetViewerDoors: %v", err)
	}
	resp := adminUnlock(t, env, "door_id=door-b")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", resp.StatusCode, readBody(t, resp))
	}
	if got != "door-b" {
		t.Errorf("opened %q, want door-b", got)
	}
}

// Zero assigned doors -> legacy paired-intercom auto-resolution.
func TestAdminUnlock_ZeroDoorsPairedFallback(t *testing.T) {
	var got string
	env := adminUnlockEnv(t, map[string]string{"door-paired": "aabbccddeeff"}, &got)
	if err := env.viewerMgr.SetPairedIntercomMAC(context.Background(), testViewerMAC,
		"aa:bb:cc:dd:ee:ff"); err != nil {
		t.Fatalf("SetPairedIntercomMAC: %v", err)
	}
	resp := adminUnlock(t, env, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (paired fallback), body=%s", resp.StatusCode, readBody(t, resp))
	}
	if got != "door-paired" {
		t.Errorf("opened %q, want door-paired (resolved via intercom)", got)
	}
}
