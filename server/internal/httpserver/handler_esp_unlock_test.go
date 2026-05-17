// Saison 14-01-FIX02 tests: /esp/unlock door-resolution paths.
//
// Four scenarios from the briefing acceptance grid:
//
//   - body.door_id explicit -> use it, never touch uaapi list
//   - body empty + paired_intercom_mac set -> uaapi.LookupDoorForIntercom
//   - body empty + viewer has no paired_intercom_mac -> 400
//   - body empty + paired_intercom but unbound -> 400
//
// The UA stub answers two unrelated paths so a single httptest
// server covers both the lookup and the unlock call.
package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"carvilon.local/server/internal/uaapi"
)

// pairedIntercomForTest is the MAC the unlock tests pin a viewer
// against. The matching door_thumbnail strings encode the lower-
// case hex form ("28704e31e29c"); LookupDoorForIntercom parses it
// back into the colon-form we store on the viewer row.
const pairedIntercomForTest = "28:70:4e:31:e2:9c"

// uaUnlockStub builds a small httptest server that mimics the
// UA-API endpoints relevant to /esp/unlock:
//
//	GET  /api/v1/developer/doors            list with optional bindings
//	PUT  /api/v1/developer/doors/<id>/unlock  success envelope
//
// listCalls + unlockCalls let each test assert which UA paths
// were actually exercised. doorListData lets each test inject
// different door bodies (or nil for an empty list).
type uaUnlockStub struct {
	srv          *httptest.Server
	listCalls    int
	unlockCalls  int
	lastUnlockID string
	doorListData []map[string]any
}

func newUAUnlockStub(t *testing.T, doors []map[string]any) *uaUnlockStub {
	t.Helper()
	stub := &uaUnlockStub{doorListData: doors}
	stub.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/developer/doors":
			stub.listCalls++
			payload, _ := json.Marshal(map[string]any{
				"code": "SUCCESS",
				"msg":  "ok",
				"data": stub.doorListData,
			})
			_, _ = w.Write(payload)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/unlock"):
			stub.unlockCalls++
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/developer/doors/")
			id = strings.TrimSuffix(id, "/unlock")
			stub.lastUnlockID = id
			_, _ = w.Write([]byte(`{"code":"SUCCESS","msg":"ok","data":null}`))
		default:
			t.Errorf("UA stub: unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(stub.srv.Close)
	return stub
}

// postESPUnlock issues POST /esp/unlock with the given bearer
// token and JSON body, returning the response so the test can
// inspect status + body without each test re-spelling the
// boilerplate.
func postESPUnlock(t *testing.T, env *testEnv, token string, body any) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/esp/unlock", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /esp/unlock: %v", err)
	}
	return resp
}

func TestESPUnlock_UsesBodyDoorIDWhenProvided(t *testing.T) {
	// Empty list - if the handler accidentally calls LookupDoorForIntercom
	// it would resolve to "" and fail with 400; instead the explicit
	// door_id path must skip the lookup entirely.
	stub := newUAUnlockStub(t, []map[string]any{})

	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung A")
	env.srv.SetUAClient(uaapi.New(uaapi.Options{BaseURL: stub.srv.URL, Token: "test-token"}))

	resp := postESPUnlock(t, env, tok, map[string]any{"door_id": "explicit-uuid"})
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, raw)
	}

	if stub.listCalls != 0 {
		t.Errorf("UA list called %d times; explicit door_id must skip lookup", stub.listCalls)
	}
	if stub.unlockCalls != 1 || stub.lastUnlockID != "explicit-uuid" {
		t.Errorf("UA unlock calls=%d id=%q; want 1 explicit-uuid", stub.unlockCalls, stub.lastUnlockID)
	}

	var body struct {
		OK         bool   `json:"ok"`
		DoorID     string `json:"door_id"`
		DoorSource string `json:"door_source"`
	}
	_ = json.Unmarshal(raw, &body)
	if !body.OK || body.DoorID != "explicit-uuid" || body.DoorSource != "body" {
		t.Errorf("response = %+v, want ok+explicit-uuid+body", body)
	}
}

func TestESPUnlock_ResolvesFromPairedIntercomWhenBodyEmpty(t *testing.T) {
	// One door, bound to pairedIntercomForTest via the thumbnail
	// string LookupDoorForIntercom parses.
	stub := newUAUnlockStub(t, []map[string]any{
		{
			"id":   "door-uuid-front",
			"name": "Hauseingang",
			"extras": map[string]any{
				"door_thumbnail": "/preview/reader_28704e31e29c_door-uuid-front_1747.jpg",
			},
		},
	})

	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung A")
	if err := env.mockMgr.SetPairedIntercomMAC(context.Background(), espTestMAC, pairedIntercomForTest); err != nil {
		t.Fatalf("set paired intercom: %v", err)
	}
	env.srv.SetUAClient(uaapi.New(uaapi.Options{BaseURL: stub.srv.URL, Token: "test-token"}))

	resp := postESPUnlock(t, env, tok, map[string]any{"event_id": "evt_xyz"})
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, raw)
	}

	if stub.listCalls != 1 {
		t.Errorf("UA list calls = %d, want 1", stub.listCalls)
	}
	if stub.unlockCalls != 1 || stub.lastUnlockID != "door-uuid-front" {
		t.Errorf("UA unlock calls=%d id=%q; want 1 door-uuid-front", stub.unlockCalls, stub.lastUnlockID)
	}

	var body struct {
		OK         bool   `json:"ok"`
		DoorID     string `json:"door_id"`
		DoorSource string `json:"door_source"`
	}
	_ = json.Unmarshal(raw, &body)
	if !body.OK || body.DoorID != "door-uuid-front" || body.DoorSource != "auto" {
		t.Errorf("response = %+v, want ok+door-uuid-front+auto", body)
	}
}

func TestESPUnlock_400WhenNoPairedAndNoBodyDoorID(t *testing.T) {
	stub := newUAUnlockStub(t, []map[string]any{})

	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung A")
	// No SetPairedIntercomMAC -> viewer.paired_intercom_mac stays empty.
	env.srv.SetUAClient(uaapi.New(uaapi.Options{BaseURL: stub.srv.URL, Token: "test-token"}))

	resp := postESPUnlock(t, env, tok, map[string]any{})
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, body=%s; want 400", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "no paired intercom configured") {
		t.Errorf("body = %q, want 'no paired intercom configured'", raw)
	}
	if stub.listCalls != 0 || stub.unlockCalls != 0 {
		t.Errorf("UA stub should never be called when no paired intercom; list=%d unlock=%d",
			stub.listCalls, stub.unlockCalls)
	}
}

func TestESPUnlock_400WhenPairedButUaapiReturnsEmpty(t *testing.T) {
	// A door exists, but its thumbnail points at a different intercom,
	// so LookupDoorForIntercom returns "" (== not-bound, not an error).
	stub := newUAUnlockStub(t, []map[string]any{
		{
			"id": "door-other",
			"extras": map[string]any{
				"door_thumbnail": "/preview/reader_aabbccddeeff_door-other_1.jpg",
			},
		},
	})

	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung A")
	if err := env.mockMgr.SetPairedIntercomMAC(context.Background(), espTestMAC, pairedIntercomForTest); err != nil {
		t.Fatalf("set paired intercom: %v", err)
	}
	env.srv.SetUAClient(uaapi.New(uaapi.Options{BaseURL: stub.srv.URL, Token: "test-token"}))

	resp := postESPUnlock(t, env, tok, map[string]any{})
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, body=%s; want 400", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "paired intercom not assigned to any door") {
		t.Errorf("body = %q, want 'paired intercom not assigned to any door'", raw)
	}
	if stub.listCalls != 1 {
		t.Errorf("UA list calls = %d, want 1", stub.listCalls)
	}
	if stub.unlockCalls != 0 {
		t.Errorf("UA unlock should not run after empty lookup; calls=%d", stub.unlockCalls)
	}
}
