package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"unifix.local/server/internal/eventbus"
	"unifix.local/server/internal/uaapi"
)

// eventbusEvent is a tiny test-only helper so the test file
// does not have to construct eventbus.Event literals all over.
func eventbusEvent(typ, data string) eventbus.Event {
	return eventbus.Event{Type: typ, JSON: data}
}

// adoptESPForTest discovers + adopts an ESP-Viewer and returns
// the freshly issued bearer token (clear-text, picked up via
// the status-poll path the way a real ESP would).
func adoptESPForTest(t *testing.T, env *testEnv, mac, name string) string {
	t.Helper()
	postDiscover(t, env, map[string]any{
		"mac":        mac,
		"model":      "UA-Display",
		"fw_version": "1.0.0",
	}).Body.Close()
	body, _ := json.Marshal(map[string]any{"mac": mac, "name": name})
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/a/esp-viewers/adopt", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	resp.Body.Close()
	_, sb := getStatus(t, env, mac)
	tok, _ := sb["token"].(string)
	if tok == "" {
		t.Fatalf("no token returned for %s", mac)
	}
	return tok
}

// invokeBearer wraps the requireESPBearer middleware around a
// dummy 200-handler and runs it with the given Authorization
// header value. Returns status code and the resolved MAC (empty
// if 401).
func invokeBearer(t *testing.T, env *testEnv, authHeader string) (int, string) {
	t.Helper()
	var seenMAC string
	h := env.srv.requireESPBearer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMAC = ESPMACFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/esp/probe", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, seenMAC
}

func TestESPAuth_RejectsMissingToken(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	code, mac := invokeBearer(t, env, "")
	if code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", code)
	}
	if mac != "" {
		t.Errorf("MAC = %q, want empty", mac)
	}
}

func TestESPAuth_RejectsInvalidToken(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	code, mac := invokeBearer(t, env, "Bearer bogus-not-a-real-token")
	if code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", code)
	}
	if mac != "" {
		t.Errorf("MAC = %q, want empty", mac)
	}
}

func TestESPAuth_AcceptsValidToken(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung A")

	code, mac := invokeBearer(t, env, "Bearer "+tok)
	if code != http.StatusOK {
		t.Errorf("status = %d, want 200", code)
	}
	if mac != espTestMAC {
		t.Errorf("MAC = %q, want %q", mac, espTestMAC)
	}

	// LookupESPMACByToken-Sanity.
	got, err := env.mockMgr.LookupESPMACByToken(context.Background(), tok)
	if err != nil {
		t.Fatalf("LookupESPMACByToken: %v", err)
	}
	if got != espTestMAC {
		t.Errorf("Lookup returned %s, want %s", got, espTestMAC)
	}
}

func TestESPConfig_ReturnsAllFields(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung A")

	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/esp/config", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("GET /esp/config: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["mieter_name"] != "Wohnung A" {
		t.Errorf("mieter_name = %v, want Wohnung A", got["mieter_name"])
	}
	for _, key := range []string{"location_name", "stream", "doors", "cameras", "ui"} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing field %q", key)
		}
	}
	stream, _ := got["stream"].(map[string]any)
	for _, k := range []string{"url", "type", "auth_header", "fallback_url"} {
		if _, ok := stream[k]; !ok {
			t.Errorf("missing stream.%s", k)
		}
	}
	ui, _ := got["ui"].(map[string]any)
	for _, k := range []string{"language", "screensaver_after_sec", "brightness_idle"} {
		if _, ok := ui[k]; !ok {
			t.Errorf("missing ui.%s", k)
		}
	}
}

// startSSE opens an SSE stream for the given bearer and returns
// the underlying *http.Response plus a cancel func. The caller
// reads the body to consume events. The response context is
// cancelled when the test ends.
func startSSE(t *testing.T, env *testEnv, tok string) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, env.ts.URL+"/esp/events", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := env.client.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET /esp/events: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		resp.Body.Close()
		cancel()
		t.Fatalf("Content-Type = %q", ct)
	}
	return resp, cancel
}

// readSSEEvent reads one event: <name>\ndata: <data>\n\n frame.
// Returns ("", "") on read error / EOF.
func readSSEEvent(t *testing.T, resp *http.Response) (eventName, data string) {
	t.Helper()
	buf := make([]byte, 1)
	var line strings.Builder
	var lines []string
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				lines = append(lines, line.String())
				line.Reset()
				// Blank line marks end of an event.
				if len(lines) >= 2 && lines[len(lines)-1] == "" {
					var e, d string
					for _, l := range lines {
						if strings.HasPrefix(l, "event: ") {
							e = strings.TrimPrefix(l, "event: ")
						} else if strings.HasPrefix(l, "data: ") {
							d = strings.TrimPrefix(l, "data: ")
						}
					}
					return e, d
				}
				continue
			}
			line.WriteByte(buf[0])
		}
		if err != nil {
			return "", ""
		}
	}
}

func TestESPEvents_SendsHeartbeat(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	// Heartbeat-Tick auf 50ms drosseln damit der Test schnell laeuft.
	env.srv.eventsHeartbeat = 50 * time.Millisecond
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung A")

	resp, cancel := startSSE(t, env, tok)
	defer cancel()
	defer resp.Body.Close()

	// Erstes Event ist der Initial-Heartbeat. Zweites Event sollte
	// nach <= 200ms vom Ticker kommen.
	name, _ := readSSEEvent(t, resp)
	if name != "heartbeat" {
		t.Fatalf("initial event = %q, want heartbeat", name)
	}
	done := make(chan struct{})
	var second string
	go func() {
		second, _ = readSSEEvent(t, resp)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("second heartbeat never arrived")
	}
	if second != "heartbeat" {
		t.Errorf("second event = %q, want heartbeat", second)
	}
}

func TestESPEvents_DeliversPublishedEvent(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.srv.eventsHeartbeat = 10 * time.Second // keep heartbeat out of the way
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung A")

	resp, cancel := startSSE(t, env, tok)
	defer cancel()
	defer resp.Body.Close()

	// Consume initial heartbeat.
	if name, _ := readSSEEvent(t, resp); name != "heartbeat" {
		t.Fatalf("initial event = %q, want heartbeat", name)
	}

	// Publish from the test side.
	env.srv.EventBus().Publish(espTestMAC, eventbusEvent("doorbell.ring", `{"event_id":"evt_x"}`))

	done := make(chan struct{})
	var ev, data string
	go func() {
		ev, data = readSSEEvent(t, resp)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("published event never arrived")
	}
	if ev != "doorbell.ring" {
		t.Errorf("event = %q, want doorbell.ring", ev)
	}
	if !strings.Contains(data, "evt_x") {
		t.Errorf("data = %q", data)
	}
}

func TestESPEvents_HandlesMultipleClients(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	env.srv.eventsHeartbeat = 10 * time.Second
	tokA := adoptESPForTest(t, env, espTestMAC, "Wohnung A")
	tokB := adoptESPForTest(t, env, "0c:ea:14:bb:cc:dd", "Wohnung B")

	respA, cancelA := startSSE(t, env, tokA)
	defer cancelA()
	defer respA.Body.Close()
	respB, cancelB := startSSE(t, env, tokB)
	defer cancelB()
	defer respB.Body.Close()

	// Drain initial heartbeats.
	for _, r := range []*http.Response{respA, respB} {
		if name, _ := readSSEEvent(t, r); name != "heartbeat" {
			t.Fatalf("initial event = %q", name)
		}
	}

	// Wait until both subscriptions are registered with the bus.
	bus := env.srv.EventBus()
	deadline := time.Now().Add(2 * time.Second)
	for bus.SubscriberCount(espTestMAC) == 0 || bus.SubscriberCount("0c:ea:14:bb:cc:dd") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("subscribers never registered")
		}
		time.Sleep(20 * time.Millisecond)
	}

	bus.Publish(espTestMAC, eventbusEvent("doorbell.ring", `{"who":"A"}`))
	bus.Publish("0c:ea:14:bb:cc:dd", eventbusEvent("doorbell.ring", `{"who":"B"}`))

	readNext := func(r *http.Response) (string, string) {
		ch := make(chan struct{}, 1)
		var n, d string
		go func() { n, d = readSSEEvent(t, r); ch <- struct{}{} }()
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatal("event missed")
		}
		return n, d
	}

	_, dA := readNext(respA)
	_, dB := readNext(respB)
	if !strings.Contains(dA, `"who":"A"`) {
		t.Errorf("client A got %q", dA)
	}
	if !strings.Contains(dB, `"who":"B"`) {
		t.Errorf("client B got %q", dB)
	}
}

func TestESPAnswer_PushesCancelToSiblings(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)

	// Zwei ESPs am selben UA-User. Sibling = B aus A's Sicht.
	tokA := adoptESPForTest(t, env, espTestMAC, "Wohnung A")
	_ = adoptESPForTest(t, env, "0c:ea:14:bb:cc:dd", "Wohnung A2")
	if err := env.mockMgr.SetLinkedUAUserID(context.Background(), espTestMAC, "u1"); err != nil {
		t.Fatalf("link A: %v", err)
	}
	if err := env.mockMgr.SetLinkedUAUserID(context.Background(), "0c:ea:14:bb:cc:dd", "u1"); err != nil {
		t.Fatalf("link B: %v", err)
	}

	// B subscribed via Bus direkt (statt SSE).
	bus := env.srv.EventBus()
	bCh := bus.Subscribe("0c:ea:14:bb:cc:dd")
	defer bus.Unsubscribe("0c:ea:14:bb:cc:dd", bCh)

	// A schickt "answer".
	body, _ := json.Marshal(map[string]any{
		"event_id": "evt_abc",
		"action":   "answer",
	})
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/esp/answer", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokA)
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /esp/answer: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := readBodyBytes(resp)
		t.Fatalf("status = %d, body=%s", resp.StatusCode, raw)
	}

	// B sollte einen doorbell.cancel-Event sehen.
	select {
	case ev := <-bCh:
		if ev.Type != "doorbell.cancel" {
			t.Errorf("ev.Type = %q, want doorbell.cancel", ev.Type)
		}
		if !strings.Contains(ev.JSON, "answered_elsewhere") {
			t.Errorf("ev.JSON = %q, want reason answered_elsewhere", ev.JSON)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sibling never received cancel event")
	}
}

// Saison 13-08: dedicated /esp/reject endpoint - mirrors
// /einloggen/reject (doorbellcalls.MarkRejected + sibling
// cancel + UDM ring-stop via call_admin_result).

func TestESPReject_PushesCancelAndMarksRejected(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung A")

	const intercomMAC = "28704e31e29c"
	if err := env.srv.calls.Start(context.Background(), "tok-esp-rej", espTestMAC, intercomMAC); err != nil {
		t.Fatalf("calls.Start: %v", err)
	}

	bus := env.srv.EventBus()
	sub := bus.Subscribe(espTestMAC)
	defer bus.Unsubscribe(espTestMAC, sub)

	body, _ := json.Marshal(map[string]any{"event_id": "tok-esp-rej"})
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/esp/reject", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /esp/reject: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := readBodyBytes(resp)
		t.Fatalf("status = %d, body=%s", resp.StatusCode, raw)
	}

	// Own MAC sees the cancel via the bus.
	select {
	case ev := <-sub:
		if ev.Type != "doorbell.cancel" {
			t.Errorf("ev.Type = %q, want doorbell.cancel", ev.Type)
		}
		if !strings.Contains(ev.JSON, "rejected") {
			t.Errorf("ev.JSON = %q, want reason rejected", ev.JSON)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rejecter did not receive its own cancel")
	}

	// doorbellcalls row updated.
	got, err := env.srv.calls.Get(context.Background(), "tok-esp-rej")
	if err != nil {
		t.Fatalf("calls.Get: %v", err)
	}
	if got.CancelReason != "rejected" {
		t.Errorf("CancelReason = %q, want rejected", got.CancelReason)
	}
	// Saison 13-09: ESP-type viewers spawn the same Mock-Goroutine
	// stack as web-type viewers, so notifyUDMReject reaches the
	// running viewer's RejectDoorbell hook and the test fake
	// captures it. Pre-S13-09 this assertion would have been
	// impossible because the goroutine never started.
	v, err := env.mockMgr.LookupForReject(espTestMAC)
	if err != nil {
		t.Fatalf("LookupForReject: %v", err)
	}
	nv := v.(*noopViewer)
	nv.rejectMu.Lock()
	defer nv.rejectMu.Unlock()
	if len(nv.rejectCalls) != 1 {
		t.Fatalf("RejectDoorbell calls = %d, want 1 (S13-09 hybrid spawn)",
			len(nv.rejectCalls))
	}
	if nv.rejectCalls[0].IntercomMAC != intercomMAC {
		t.Errorf("intercom = %q, want %q",
			nv.rejectCalls[0].IntercomMAC, intercomMAC)
	}
}

func TestESPReject_StaleCallReturnsOK(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung A")

	body, _ := json.Marshal(map[string]any{"event_id": "tok-not-active"})
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/esp/reject", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /esp/reject: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (stale = ok)", resp.StatusCode)
	}
	var payload map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	if payload["note"] != "stale" {
		t.Errorf("note = %v, want 'stale'", payload["note"])
	}
}

func TestESPReject_RequiresBearer(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	body, _ := json.Marshal(map[string]any{"event_id": "tok-x"})
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/esp/reject", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /esp/reject: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestESPUnlock_CallsUAAPIUnlock(t *testing.T) {
	var gotDoorID, gotActorID string
	uaStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// PUT /api/v1/developer/doors/{id}/unlock
		gotDoorID = strings.TrimPrefix(r.URL.Path, "/api/v1/developer/doors/")
		gotDoorID = strings.TrimSuffix(gotDoorID, "/unlock")
		var body struct {
			ActorID   string `json:"actor_id"`
			ActorName string `json:"actor_name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotActorID = body.ActorID
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"SUCCESS","msg":"ok","data":null}`))
	}))
	defer uaStub.Close()

	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung A")

	// UA-Client auf den Stub umlenken.
	env.srv.SetUAClient(uaapi.New(uaapi.Options{BaseURL: uaStub.URL, Token: "test-token"}))

	body, _ := json.Marshal(map[string]any{
		"door_id":  "door-hub-1",
		"event_id": "evt_abc",
	})
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/esp/unlock", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /esp/unlock: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := readBodyBytes(resp)
		t.Fatalf("status = %d, body=%s", resp.StatusCode, raw)
	}
	if gotDoorID != "door-hub-1" {
		t.Errorf("UA-API saw door_id = %q, want door-hub-1", gotDoorID)
	}
	if gotActorID != espTestMAC {
		t.Errorf("UA-API saw actor_id = %q, want %q", gotActorID, espTestMAC)
	}
}

// Saison 13-08 Phase A: /esp/stream.mjpeg reverse-proxy stub.

func TestESPStream_RequiresBearer(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	resp, err := env.client.Get(env.ts.URL + "/esp/stream.mjpeg")
	if err != nil {
		t.Fatalf("GET /esp/stream.mjpeg: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestESPStream_503WhenBackendUnconfigured(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung A")

	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/esp/stream.mjpeg", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("GET /esp/stream.mjpeg: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (backend unconfigured)", resp.StatusCode)
	}
}

func TestESPStream_ForwardsToBackendWithoutAuthHeader(t *testing.T) {
	var sawAuth string
	var sawPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		sawPath = r.URL.Path + "?" + r.URL.RawQuery
		w.Header().Set("Content-Type", "multipart/x-mixed-replace;boundary=frame")
		_, _ = w.Write([]byte("frame-bytes"))
	}))
	defer backend.Close()

	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung A")

	// Inject the backend URL via the running server's config.
	env.srv.cfg.StreamBackendURL = backend.URL + "/api/stream.mjpeg?src=front"

	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/esp/stream.mjpeg", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("GET /esp/stream.mjpeg: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if sawAuth != "" {
		t.Errorf("backend saw Authorization header %q; should be stripped", sawAuth)
	}
	if sawPath != "/api/stream.mjpeg?src=front" {
		t.Errorf("backend saw path %q, want /api/stream.mjpeg?src=front", sawPath)
	}
}

func TestESPState_StoresLatestReport(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	tok := adoptESPForTest(t, env, espTestMAC, "Wohnung A")

	body, _ := json.Marshal(map[string]any{
		"screen":         "idle",
		"last_input_ts":  1778684500,
		"uptime_sec":     3600,
	})
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/esp/state", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /esp/state: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got, ok := env.srv.ESPState(espTestMAC)
	if !ok {
		t.Fatal("no state recorded")
	}
	if got.Screen != "idle" || got.UptimeSec != 3600 || got.LastInputTS != 1778684500 {
		t.Errorf("state = %+v", got)
	}
}

// readBodyBytes is a small reader helper that does not consume
// the body via the existing readBody util (which returns string
// and closes already).
func readBodyBytes(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return []byte(sb.String()), nil
}
