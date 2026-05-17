package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"carvilon.local/server/internal/doorbellcalls"
	"carvilon.local/server/internal/eventbus"
	"carvilon.local/server/internal/uaapi"
)

// uaDoorStubConfig drives the test-side UA-API stub used by all
// the saison-13-07 mieter-unlock tests. Each entry maps a door
// UUID to the intercom MAC its extras.door_thumbnail encodes.
type uaDoorStubConfig struct {
	doors map[string]string // door-uuid -> intercom-mac (lowercase, no colons in path)
}

// newUADoorsStub returns an httptest.Server that satisfies both
//
//	GET /api/v1/developer/doors            (LookupDoorForIntercom)
//	PUT /api/v1/developer/doors/{id}/unlock (UnlockDoor)
//
// gotDoorID is captured by the unlock branch so the test can
// assert which door we actually attempted to open.
func newUADoorsStub(t *testing.T, cfg uaDoorStubConfig, gotDoorID *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/developer/doors":
			rows := make([]map[string]any, 0, len(cfg.doors))
			for doorID, intercomBare := range cfg.doors {
				rows = append(rows, map[string]any{
					"id":   doorID,
					"name": "Door " + doorID,
					"extras": map[string]any{
						"door_thumbnail": "/preview/reader_" + intercomBare + "_" + doorID + "_1.jpg",
					},
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": "SUCCESS", "msg": "ok", "data": rows,
			})
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/unlock"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/developer/doors/"), "/unlock")
			if gotDoorID != nil {
				*gotDoorID = id
			}
			_, _ = w.Write([]byte(`{"code":"SUCCESS","msg":"ok","data":null}`))
		default:
			http.Error(w, "ua stub: unhandled "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
}

// loginMieterForTest seeds a viewer and signs the test client
// into a viewer session by posting username + password.
func loginMieterForTest(t *testing.T, env *testEnv) {
	t.Helper()
	env.seedViewer(t)
	resp := env.loginViewer(t, testViewerLogin, testViewerPassword)
	resp.Body.Close()
}

// Saison 13-07: bell-overlay path. The browser POSTs the
// intercom MAC (colon-form, from doorbell_start.device_id) and
// the handler auto-resolves the door via uaapi.LookupDoorForIntercom.
func TestMieterUnlock_BellOverlayResolvesViaThumbnail(t *testing.T) {
	var gotDoorID string
	uaStub := newUADoorsStub(t, uaDoorStubConfig{
		doors: map[string]string{
			"door-uuid-front": "28704e31e29c",
			"door-uuid-back":  "aabbccddeeff",
		},
	}, &gotDoorID)
	defer uaStub.Close()

	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env)
	env.srv.SetUAClient(uaapi.New(uaapi.Options{BaseURL: uaStub.URL, Token: "t"}))

	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/webviewer/doors/28:70:4e:31:e2:9c/unlock", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}
	if gotDoorID != "door-uuid-front" {
		t.Errorf("UA-API saw door = %q, want door-uuid-front", gotDoorID)
	}

	// Audit row written.
	var n int
	if err := env.d.QueryRow(
		`SELECT COUNT(*) FROM door_events WHERE event_type = 'door_unlocked' AND viewer_mac = ?`,
		testViewerMAC,
	).Scan(&n); err != nil {
		t.Fatalf("door_events count: %v", err)
	}
	if n != 1 {
		t.Errorf("door_events count = %d, want 1", n)
	}
}

// Saison 13-07: bell-overlay path with bare 12-hex from the SSE
// frame (saison-13-05-HOTFIX5 verified the SSE device_id format).
func TestMieterUnlock_BareMACPathResolvesViaThumbnail(t *testing.T) {
	var gotDoorID string
	uaStub := newUADoorsStub(t, uaDoorStubConfig{
		doors: map[string]string{
			"door-uuid-front": "28704e31e29c",
		},
	}, &gotDoorID)
	defer uaStub.Close()

	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env)
	env.srv.SetUAClient(uaapi.New(uaapi.Options{BaseURL: uaStub.URL, Token: "t"}))

	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/webviewer/doors/28704e31e29c/unlock", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}
	if gotDoorID != "door-uuid-front" {
		t.Errorf("UA-API saw door = %q, want door-uuid-front", gotDoorID)
	}
}

// Saison 13-07: standby path. The home screen POSTs to the
// literal /webviewer/doors/standby/unlock; the handler reads the
// viewer's paired_intercom_mac and resolves from there.
func TestMieterUnlock_StandbyUsesPairedIntercom(t *testing.T) {
	var gotDoorID string
	uaStub := newUADoorsStub(t, uaDoorStubConfig{
		doors: map[string]string{
			"door-uuid-front": "28704e31e29c",
		},
	}, &gotDoorID)
	defer uaStub.Close()

	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env)
	env.srv.SetUAClient(uaapi.New(uaapi.Options{BaseURL: uaStub.URL, Token: "t"}))

	if err := env.mockMgr.SetPairedIntercomMAC(context.Background(),
		testViewerMAC, "28:70:4e:31:e2:9c"); err != nil {
		t.Fatalf("SetPairedIntercomMAC: %v", err)
	}

	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/webviewer/doors/standby/unlock", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body=%s", resp.StatusCode, readBody(t, resp))
	}
	if gotDoorID != "door-uuid-front" {
		t.Errorf("UA-API saw door = %q, want door-uuid-front", gotDoorID)
	}
}

func TestMieterUnlock_StandbyWithoutPairingReturns404(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env)
	env.srv.SetUAClient(uaapi.New(uaapi.Options{BaseURL: "http://invalid", Token: "t"}))

	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/webviewer/doors/standby/unlock", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (no paired intercom)", resp.StatusCode)
	}
}

func TestMieterUnlock_IntercomNotBoundReturns404(t *testing.T) {
	// Stub returns a door whose thumbnail points at SOMEONE ELSE's
	// intercom; the lookup must come back empty for the requested MAC.
	uaStub := newUADoorsStub(t, uaDoorStubConfig{
		doors: map[string]string{
			"door-uuid-back": "aabbccddeeff",
		},
	}, nil)
	defer uaStub.Close()

	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env)
	env.srv.SetUAClient(uaapi.New(uaapi.Options{BaseURL: uaStub.URL, Token: "t"}))

	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/webviewer/doors/28:70:4e:31:e2:9c/unlock", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (intercom not bound to a door)", resp.StatusCode)
	}
}

func TestMieterUnlock_BadPathParamReturns400(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env)
	env.srv.SetUAClient(uaapi.New(uaapi.Options{BaseURL: "http://invalid", Token: "t"}))

	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/webviewer/doors/garbage/unlock", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestMieterAnswer_FirstWinsPushesCancelToOthers(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env)

	// Aktiver Anruf vorbereiten (sonst gibt MarkAnswered Not-Found).
	calls := env.srv.calls
	if err := calls.Start(context.Background(), "tok-call-1", testViewerMAC, "intercom-x"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Zweiten Browser-Tab simulieren via EventBus-Sub.
	bus := env.srv.EventBus()
	other := bus.Subscribe(testViewerMAC)
	defer bus.Unsubscribe(testViewerMAC, other)

	body, _ := json.Marshal(map[string]any{"event_id": "tok-call-1"})
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/webviewer/answer", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /webviewer/answer: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw := readBody(t, resp)
		t.Fatalf("status = %d, body=%s", resp.StatusCode, raw)
	}

	// other-Subscriber bekommt doorbell.cancel mit reason answered_elsewhere.
	select {
	case ev := <-other:
		if ev.Type != "doorbell.cancel" {
			t.Errorf("ev.Type = %q", ev.Type)
		}
		if !strings.Contains(ev.JSON, "answered_elsewhere") {
			t.Errorf("ev.JSON = %q", ev.JSON)
		}
	default:
		t.Fatal("other subscriber did not get cancel")
	}

	// Zweiter Klick auf Answer ist 409.
	req2, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/webviewer/answer", bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := env.client.Do(req2)
	if err != nil {
		t.Fatalf("second answer: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("second answer status = %d, want 409", resp2.StatusCode)
	}
}

func TestMieterReject_CancelForAll(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env)
	if err := env.srv.calls.Start(context.Background(), "tok-rej", testViewerMAC, "intercom-x"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	bus := env.srv.EventBus()
	sub := bus.Subscribe(testViewerMAC)
	defer bus.Unsubscribe(testViewerMAC, sub)

	body, _ := json.Marshal(map[string]any{"event_id": "tok-rej"})
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/webviewer/reject", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /webviewer/reject: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}

	select {
	case ev := <-sub:
		if !strings.Contains(ev.JSON, "rejected") {
			t.Errorf("ev.JSON = %q, want reason rejected", ev.JSON)
		}
	default:
		t.Fatal("rejecter did not receive its own cancel")
	}

	// DB-Row: cancel_reason = rejected.
	got, err := env.srv.calls.Get(context.Background(), "tok-rej")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.CancelReason != doorbellcalls.ReasonRejected {
		t.Errorf("CancelReason = %q", got.CancelReason)
	}
}

func TestMieterEndCall_PushesUserEnded(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env)
	if err := env.srv.calls.Start(context.Background(), "tok-end", testViewerMAC, ""); err != nil {
		t.Fatalf("Start: %v", err)
	}
	_, _ = env.srv.calls.MarkAnswered(context.Background(), "tok-end", testViewerMAC)

	bus := env.srv.EventBus()
	sub := bus.Subscribe(testViewerMAC)
	defer bus.Unsubscribe(testViewerMAC, sub)

	body, _ := json.Marshal(map[string]any{"event_id": "tok-end"})
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/webviewer/end-call", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /webviewer/end-call: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	select {
	case ev := <-sub:
		if !strings.Contains(ev.JSON, "user_ended") {
			t.Errorf("ev.JSON = %q", ev.JSON)
		}
	default:
		t.Fatal("end-call did not push cancel")
	}
}

// Forces a reference to eventbus at package level so
// unused-import lints stay quiet if the file gets trimmed.
var _ = eventbus.New

// findRunningViewer locates the noopViewer instance the test env
// spawned for the seedViewer test MAC. Used by Saison-13-04.5-B
// tests that need to inspect the recorded RejectDoorbell calls.
func findRunningViewer(t *testing.T, env *testEnv, mac string) *noopViewer {
	t.Helper()
	v, err := env.mockMgr.LookupForReject(mac)
	if err != nil {
		t.Fatalf("LookupForReject(%s): %v", mac, err)
	}
	nv, ok := v.(*noopViewer)
	if !ok {
		t.Fatalf("viewer for %s is %T, want *noopViewer", mac, v)
	}
	return nv
}

func TestMieterReject_PublishesCallAdminResultToUDM(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env)

	const intercomMAC = "28704e31e29c"
	if err := env.srv.calls.Start(context.Background(), "tok-call-admin", testViewerMAC, intercomMAC); err != nil {
		t.Fatalf("Start: %v", err)
	}

	body, _ := json.Marshal(map[string]any{"event_id": "tok-call-admin"})
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/webviewer/reject", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /webviewer/reject: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	nv := findRunningViewer(t, env, testViewerMAC)
	nv.rejectMu.Lock()
	defer nv.rejectMu.Unlock()
	if len(nv.rejectCalls) != 1 {
		t.Fatalf("RejectDoorbell calls = %d, want 1", len(nv.rejectCalls))
	}
	if nv.rejectCalls[0].IntercomMAC != intercomMAC {
		t.Errorf("intercom = %q, want %q", nv.rejectCalls[0].IntercomMAC, intercomMAC)
	}
}

func TestMieterEndCall_PublishesCallAdminResultToUDM(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env)

	const intercomMAC = "28704e31e29c"
	_ = env.srv.calls.Start(context.Background(), "tok-end-admin", testViewerMAC, intercomMAC)
	_, _ = env.srv.calls.MarkAnswered(context.Background(), "tok-end-admin", testViewerMAC)

	body, _ := json.Marshal(map[string]any{"event_id": "tok-end-admin"})
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/webviewer/end-call", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /webviewer/end-call: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	nv := findRunningViewer(t, env, testViewerMAC)
	nv.rejectMu.Lock()
	defer nv.rejectMu.Unlock()
	if len(nv.rejectCalls) != 1 {
		t.Fatalf("RejectDoorbell calls = %d, want 1", len(nv.rejectCalls))
	}
	if nv.rejectCalls[0].IntercomMAC != intercomMAC {
		t.Errorf("intercom = %q, want %q", nv.rejectCalls[0].IntercomMAC, intercomMAC)
	}
}
