package httpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"unifix.local/server/internal/doorbellhub"
	"unifix.local/server/internal/mockmanager"
)

// loginAndOpenEvents performs the username+password login flow and
// then opens the SSE stream as that viewer. Returns the bufio.Reader
// for line-by-line consumption and a cancel that closes the stream
// cleanly.
func loginAndOpenEvents(t *testing.T, env *testEnv, viewerMAC string) (*bufio.Reader, *http.Response, context.CancelFunc) {
	t.Helper()
	if _, err := env.mockMgr.GetViewerInfo(context.Background(), viewerMAC); err != nil {
		if errors.Is(err, mockmanager.ErrViewerNotFound) {
			env.seedViewerAs(t, viewerMAC, "Test Viewer", "test-"+viewerMAC[len(viewerMAC)-5:], "TestPw-1234567X")
		} else {
			t.Fatalf("GetViewerInfo: %v", err)
		}
	}
	info, _, err := env.mockMgr.LookupByUsername(context.Background(), "test-"+viewerMAC[len(viewerMAC)-5:])
	if err != nil {
		t.Fatalf("LookupByUsername: %v", err)
	}
	resp := env.loginViewer(t, info.Username, "TestPw-1234567X")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303", resp.StatusCode)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, env.ts.URL+"/einloggen/events", nil)
	if err != nil {
		cancel()
		t.Fatalf("new req: %v", err)
	}
	// Re-use the cookie jar from env.client but bypass the
	// no-follow CheckRedirect: streaming clients should never
	// follow a 303 anyway.
	streamClient := &http.Client{Jar: env.client.Jar}
	sseResp, err := streamClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open sse: %v", err)
	}
	if sseResp.StatusCode != http.StatusOK {
		sseResp.Body.Close()
		cancel()
		t.Fatalf("sse status = %d, want 200", sseResp.StatusCode)
	}
	br := bufio.NewReader(sseResp.Body)
	return br, sseResp, func() {
		cancel()
		sseResp.Body.Close()
	}
}

// nextSSEEvent reads SSE-formatted lines until it finds a
// terminating empty line, returning the parsed event-name and
// data payload. Comments (lines starting with ":") are skipped.
func nextSSEEvent(t *testing.T, br *bufio.Reader, timeout time.Duration) (string, string) {
	t.Helper()
	type result struct {
		event string
		data  string
	}
	resCh := make(chan result, 1)
	errCh := make(chan error, 1)
	go func() {
		var event, data string
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				errCh <- err
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				if event != "" || data != "" {
					resCh <- result{event, data}
					return
				}
				continue
			}
			if strings.HasPrefix(line, ":") {
				// comment / keepalive, skip
				continue
			}
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	select {
	case r := <-resCh:
		return r.event, r.data
	case err := <-errCh:
		t.Fatalf("sse read error: %v", err)
		return "", ""
	case <-time.After(timeout):
		t.Fatalf("no sse event within %v", timeout)
		return "", ""
	}
}

// ---------- Auth gating ----------

func TestEvents_RequiresSession(t *testing.T) {
	env := newTestServer(t)
	resp, err := env.client.Get(env.ts.URL + "/einloggen/events")
	if err != nil {
		t.Fatalf("GET /m/events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303 (redirect to login)", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/einloggen" {
		t.Errorf("Location = %q, want /einloggen", loc)
	}
}

// ---------- SSE headers ----------

func TestEvents_SetsSSEHeaders(t *testing.T) {
	env := newTestServer(t)
	br, resp, cancel := loginAndOpenEvents(t, env, testViewerMAC)
	defer cancel()
	_ = br
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
	if xab := resp.Header.Get("X-Accel-Buffering"); xab != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", xab)
	}
}

// ---------- Event streaming ----------

func TestEvents_StreamsDoorbellEvent(t *testing.T) {
	env := newTestServer(t)
	br, _, cancel := loginAndOpenEvents(t, env, testViewerMAC)
	defer cancel()

	// Push an event into the hub for this mock.
	env.hub.Publish(testViewerMAC, doorbellhub.Event{
		Type:      doorbellhub.TypeDoorbellStart,
		MockMAC:   testViewerMAC,
		RequestID: "req-1",
		DeviceID:  "0c:ea:14:11:11:11",
		CreatedAt: 1747000000000,
	})

	name, data := nextSSEEvent(t, br, 2*time.Second)
	if name != doorbellhub.TypeDoorbellStart {
		t.Errorf("event name = %q, want %q", name, doorbellhub.TypeDoorbellStart)
	}
	// Saison 13-02-FIX3 wire format:
	//   { "door": "...", "ts": "RFC3339", "raw": { full hub.Event } }
	// "raw" still carries the legacy MockMAC + RequestID fields so
	// tests like this one can still poke at them without parsing
	// the new shape.
	var payload struct {
		Door string            `json:"door"`
		TS   string            `json:"ts"`
		Raw  doorbellhub.Event `json:"raw"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("decode data: %v (raw: %s)", err, data)
	}
	if payload.Door == "" {
		t.Errorf("door = empty in SSE payload")
	}
	if payload.TS == "" {
		t.Errorf("ts = empty in SSE payload")
	}
	if payload.Raw.MockMAC != testViewerMAC {
		t.Errorf("raw.MockMAC = %q", payload.Raw.MockMAC)
	}
	if payload.Raw.RequestID != "req-1" {
		t.Errorf("raw.RequestID = %q", payload.Raw.RequestID)
	}
}

func TestEvents_IgnoresEventsForOtherMocks(t *testing.T) {
	env := newTestServer(t)
	br, _, cancel := loginAndOpenEvents(t, env, testViewerMAC)
	defer cancel()

	// Event addressed to a different mock must not reach our subscriber.
	env.hub.Publish("0c:ea:14:99:99:99", doorbellhub.Event{
		Type:    doorbellhub.TypeDoorbellStart,
		MockMAC: "0c:ea:14:99:99:99",
	})
	// give the hub a moment, then verify only the keepalive
	// passes through (50ms heartbeat in test config).
	time.Sleep(150 * time.Millisecond)
	// Read whatever is buffered: should be only :keepalive comments.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		t.Errorf("unexpected SSE line for other-mock event: %q", line)
		break
	}
}

// ---------- Heartbeat ----------

func TestEvents_SendsHeartbeat(t *testing.T) {
	env := newTestServer(t)
	br, _, cancel := loginAndOpenEvents(t, env, testViewerMAC)
	defer cancel()

	// The test config uses a 50 ms heartbeat. Read lines for up
	// to a second and expect at least one ":keepalive" comment.
	type result struct {
		seen bool
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				resCh <- result{seen: false, err: err}
				return
			}
			if strings.HasPrefix(line, ":keepalive") {
				resCh <- result{seen: true}
				return
			}
		}
	}()
	select {
	case r := <-resCh:
		if !r.seen {
			t.Errorf("no keepalive within timeout, err=%v", r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no keepalive observed within 2s")
	}
}

// ---------- Cleanup on disconnect ----------

func TestEvents_CleanupRunsOnClientDisconnect(t *testing.T) {
	env := newTestServer(t)
	_, _, cancel := loginAndOpenEvents(t, env, testViewerMAC)

	// Wait for the subscription to register.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if env.hub.Stats().SubscriberCount >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if env.hub.Stats().SubscriberCount == 0 {
		t.Fatal("subscriber never registered")
	}

	cancel() // Closes the stream from the client side.

	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if env.hub.Stats().SubscriberCount == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("subscriber not cleaned up after disconnect (count=%d)",
		env.hub.Stats().SubscriberCount)
}
