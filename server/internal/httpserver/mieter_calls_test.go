package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"unifix.local/server/internal/doorbellcalls"
	"unifix.local/server/internal/eventbus"
	"unifix.local/server/internal/platformconfig"
	"unifix.local/server/internal/uaapi"
)

// loginMieterForTest seeds a viewer and signs the test client
// into a viewer session by posting username + password.
func loginMieterForTest(t *testing.T, env *testEnv) {
	t.Helper()
	env.seedViewer(t)
	resp := env.loginViewer(t, testViewerLogin, testViewerPassword)
	resp.Body.Close()
}

func TestMieterUnlock_CallsUAAPIAndAudits(t *testing.T) {
	var gotDoorID string
	uaStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotDoorID = strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/developer/doors/"), "/unlock")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"SUCCESS","msg":"ok","data":null}`))
	}))
	defer uaStub.Close()

	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	loginMieterForTest(t, env)
	env.srv.SetUAClient(uaapi.New(uaapi.Options{BaseURL: uaStub.URL, Token: "t"}))

	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/einloggen/doors/door-mieter-1/unlock", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /einloggen/doors/.../unlock: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw := readBody(t, resp)
		t.Fatalf("status = %d, body=%s", resp.StatusCode, raw)
	}
	if gotDoorID != "door-mieter-1" {
		t.Errorf("UA-API saw door = %q, want door-mieter-1", gotDoorID)
	}
	// Audit row in door_events.
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
		env.ts.URL+"/einloggen/answer", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /einloggen/answer: %v", err)
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
		env.ts.URL+"/einloggen/answer", bytes.NewReader(body))
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
		env.ts.URL+"/einloggen/reject", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /einloggen/reject: %v", err)
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
		env.ts.URL+"/einloggen/end-call", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /einloggen/end-call: %v", err)
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

// Forces a reference to eventbus + platformconfig at package
// level so unused-import lints stay quiet if the file gets
// trimmed.
var (
	_ = eventbus.New
	_ = platformconfig.KeyIntercomToDoor
)
