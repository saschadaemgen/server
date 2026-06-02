package whipclient

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"carvilon.local/stream/internal/publishtoken"
)

// --- synthetic WHIP server (mirrors internal/whip S2-04 without importing it) ---

type testServer struct {
	srv     *httptest.Server
	hmacKey []byte

	mu         sync.Mutex
	answerers  []*webrtc.PeerConnection
	gotPublish atomic.Bool
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	ts := &testServer{hmacKey: bytes.Repeat([]byte{0x5A}, 32)}
	ts.srv = httptest.NewTLSServer(http.HandlerFunc(ts.handle))
	t.Cleanup(func() {
		ts.srv.Close()
		ts.mu.Lock()
		for _, pc := range ts.answerers {
			_ = pc.Close()
		}
		ts.mu.Unlock()
	})
	return ts
}

// url is the base WHIP URL; the client appends "/" + streamID.
func (ts *testServer) url() string          { return ts.srv.URL + "/whip" }
func (ts *testServer) client() *http.Client { return ts.srv.Client() }

func (ts *testServer) handle(w http.ResponseWriter, r *http.Request) {
	streamID := path.Base(r.URL.Path)
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if err := publishtoken.Verify(token, streamID, ts.hmacKey, time.Now().UTC()); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read", http.StatusBadRequest)
		return
	}

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		http.Error(w, "pc", http.StatusInternalServerError)
		return
	}
	ts.mu.Lock()
	ts.answerers = append(ts.answerers, pc)
	ts.mu.Unlock()

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(body)}); err != nil {
		http.Error(w, "offer", http.StatusInternalServerError)
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		http.Error(w, "answer", http.StatusInternalServerError)
		return
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		http.Error(w, "local", http.StatusInternalServerError)
		return
	}
	<-gather

	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", "/whip/"+streamID+"/session/testsess123")
	w.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(w, pc.LocalDescription().SDP)
	ts.gotPublish.Store(true)
}

// --- helpers ----------------------------------------------------------------

// safeBuf is a concurrency-safe log sink (worker goroutines log
// alongside the test goroutine).
type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func dummyTrackSource(streamID string) (webrtc.TrackLocal, func(), error) {
	tr, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video", streamID,
	)
	return tr, func() {}, err
}

// signToken mirrors the carvilon-edge token issuer (HMAC-SHA256 over the
// base64url payload), so the synthetic server's publishtoken.Verify
// accepts it.
func signToken(t *testing.T, sid string, key []byte) string {
	t.Helper()
	raw, err := json.Marshal(publishtoken.Payload{SID: sid, Exp: time.Now().Add(time.Minute).Unix(), Nonce: "n"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	pb := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(pb))
	return pb + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func newClient(t *testing.T, ts *testServer, src TrackSourceFunc, logger *log.Logger) *Client {
	t.Helper()
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	c, err := New(Config{
		TrackSource: src,
		HTTPClient:  ts.client(),
		Logger:      logger,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func eventually(t *testing.T, timeout time.Duration, fn func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %s", timeout, msg)
}

func sessionPresent(c *Client, streamID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.sessions[streamID]
	return ok
}

func sessionCount(c *Client) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sessions)
}

// --- tests ------------------------------------------------------------------

func TestStartPublish_Happy(t *testing.T) {
	ts := newTestServer(t)
	c := newClient(t, ts, dummyTrackSource, nil)
	const sid = "test-mac"

	c.StartPublish(sid, signToken(t, sid, ts.hmacKey), ts.url(), nil)

	// session is registered synchronously; capture its done channel.
	c.mu.Lock()
	sess := c.sessions[sid]
	c.mu.Unlock()
	if sess == nil {
		t.Fatal("StartPublish did not register a session")
	}
	done := sess.done

	// server received and answered the publish (full handshake).
	eventually(t, 5*time.Second, ts.gotPublish.Load, "server did not receive publish")
	// the worker reached the park (session persists, did not error out).
	eventually(t, 2*time.Second, func() bool { return sessionPresent(c, sid) }, "client session not held")

	// StopPublish must tear the worker down cleanly.
	c.StopPublish(sid)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not exit after StopPublish")
	}
	if sessionPresent(c, sid) {
		t.Error("session still present after worker exit")
	}
}

// TestStartPublish_WithICEServers proves the S3 TURN ICEServers param
// threads through StartPublish -> runPublish -> PeerConnection without
// breaking the publish lifecycle. The TURN server is intentionally
// unreachable (TEST-NET-1); host candidates still complete the local
// handshake. pion exposes no getter for the configured ICEServers (same
// limitation the edge-side tests have), so this asserts the lifecycle,
// not the PC config field.
func TestStartPublish_WithICEServers(t *testing.T) {
	ts := newTestServer(t)
	c := newClient(t, ts, dummyTrackSource, nil)
	// c.Close blocks until every worker exits; with an unreachable TURN
	// pion's relay-agent drain inside pc.Close can take a few seconds, so
	// we rely on Cleanup for leak-free teardown rather than asserting a
	// teardown deadline (that timing is pion's, not ours).
	t.Cleanup(func() { _ = c.Close() })
	const sid = "turn-mac"

	ice := []webrtc.ICEServer{{
		URLs:       []string{"turn:192.0.2.1:3478?transport=udp"}, // TEST-NET-1, unreachable
		Username:   "u",
		Credential: "p",
	}}
	c.StartPublish(sid, signToken(t, sid, ts.hmacKey), ts.url(), ice)

	// Registration is synchronous (before the worker goroutine), so this
	// proves the S3 ICEServers param is accepted and threaded through
	// StartPublish without breaking the call path. pion exposes no getter
	// for the configured ICEServers (the same limit the edge tests have).
	if !sessionPresent(c, sid) {
		t.Fatal("StartPublish with ICEServers did not register a session")
	}
}

func TestStartPublish_DoublePublish(t *testing.T) {
	ts := newTestServer(t)
	var buf safeBuf
	c := newClient(t, ts, dummyTrackSource, log.New(&buf, "", 0))
	const sid = "test-mac"
	tok := signToken(t, sid, ts.hmacKey)

	c.StartPublish(sid, tok, ts.url(), nil)
	eventually(t, 2*time.Second, func() bool { return sessionPresent(c, sid) }, "first session not created")

	c.StartPublish(sid, tok, ts.url(), nil) // must be ignored

	if n := sessionCount(c); n != 1 {
		t.Errorf("session count = %d, want 1 after double publish", n)
	}
	if !strings.Contains(buf.String(), "already active") {
		t.Errorf("expected 'already active' log, got: %s", buf.String())
	}
}

func TestStartPublish_TrackSourceFails(t *testing.T) {
	ts := newTestServer(t)
	failSrc := func(string) (webrtc.TrackLocal, func(), error) {
		return nil, nil, errors.New("source boom")
	}
	c := newClient(t, ts, failSrc, nil)
	const sid = "test-mac"

	c.StartPublish(sid, signToken(t, sid, ts.hmacKey), ts.url(), nil)

	// worker fails fast and removes its own session — no leak.
	eventually(t, 2*time.Second, func() bool { return !sessionPresent(c, sid) },
		"session not cleaned up after track-source failure")
}

func TestStartPublish_ServerReturns401(t *testing.T) {
	ts := newTestServer(t)
	c := newClient(t, ts, dummyTrackSource, nil)
	const sid = "test-mac"

	// Garbage token -> server's publishtoken.Verify fails -> 401.
	c.StartPublish(sid, "garbage.token", ts.url(), nil)

	eventually(t, 4*time.Second, func() bool { return !sessionPresent(c, sid) },
		"session not cleaned up after 401")
}

func TestStopPublish_Unknown(t *testing.T) {
	ts := newTestServer(t)
	c := newClient(t, ts, dummyTrackSource, nil)
	// Must not panic; pure no-op.
	c.StopPublish("never-started")
}

// TestStopPublish_CallsTrackStop is the S2-06 behaviour guarantee: the
// worker invokes the TrackSource's stop function on teardown, so the
// underlying source (and its shared upstream pull) is released.
func TestStopPublish_CallsTrackStop(t *testing.T) {
	ts := newTestServer(t)
	var stopped atomic.Bool
	src := func(streamID string) (webrtc.TrackLocal, func(), error) {
		tr, err := webrtc.NewTrackLocalStaticRTP(
			webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", streamID)
		return tr, func() { stopped.Store(true) }, err
	}
	c := newClient(t, ts, src, nil)
	const sid = "test-mac"

	c.StartPublish(sid, signToken(t, sid, ts.hmacKey), ts.url(), nil)
	eventually(t, 2*time.Second, func() bool { return sessionPresent(c, sid) }, "session not created")

	c.StopPublish(sid)
	eventually(t, 3*time.Second, stopped.Load, "track stop function was not called on teardown")
}

func TestClose_TerminatesAllSessions(t *testing.T) {
	ts := newTestServer(t)
	c, err := New(Config{
		TrackSource: dummyTrackSource,
		HTTPClient:  ts.client(),
		Logger:      log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	c.StartPublish("a", signToken(t, "a", ts.hmacKey), ts.url(), nil)
	c.StartPublish("b", signToken(t, "b", ts.hmacKey), ts.url(), nil)
	eventually(t, 3*time.Second, func() bool { return sessionCount(c) == 2 }, "two sessions not active")

	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if n := sessionCount(c); n != 0 {
		t.Errorf("sessions after Close = %d, want 0", n)
	}
}

func TestNew_RequiresTrackSource(t *testing.T) {
	if _, err := New(Config{Logger: log.New(io.Discard, "", 0)}); err == nil {
		t.Error("New without TrackSource = nil error, want error")
	}
}

func TestNew_RequiresLogger(t *testing.T) {
	if _, err := New(Config{TrackSource: dummyTrackSource}); err == nil {
		t.Error("New without Logger = nil error, want error")
	}
}

// TestOnICEState_FiresStructuredEvent drives a real publish against the
// loopback pion answerer; both ends are local so ICE progresses and the
// optional OnICEState callback fires with a well-formed event (matching
// streamID, a non-empty state string, a non-negative elapsed). This proves
// the structured ICE-history hook, in addition to the existing log line.
func TestOnICEState_FiresStructuredEvent(t *testing.T) {
	ts := newTestServer(t)

	var mu sync.Mutex
	var events []ICEStateEvent
	c, err := New(Config{
		TrackSource: dummyTrackSource,
		HTTPClient:  ts.client(),
		Logger:      log.New(io.Discard, "", 0),
		OnICEState: func(e ICEStateEvent) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	const sid = "cam-ice"
	c.StartPublish(sid, signToken(t, sid, ts.hmacKey), ts.url(), nil)

	eventually(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(events) > 0
	}, "expected at least one OnICEState event")

	mu.Lock()
	defer mu.Unlock()
	for _, e := range events {
		if e.StreamID != sid {
			t.Errorf("event StreamID = %q, want %q", e.StreamID, sid)
		}
		if e.State == "" {
			t.Errorf("event has an empty State string")
		}
		if e.SinceStart < 0 {
			t.Errorf("event SinceStart = %v, want >= 0", e.SinceStart)
		}
	}
}

// TestOnICEState_NilIsLogOnly proves a nil OnICEState does not break the
// publish path (today's log-only behaviour stays intact).
func TestOnICEState_NilIsLogOnly(t *testing.T) {
	ts := newTestServer(t)
	c := newClient(t, ts, dummyTrackSource, nil) // newClient sets no OnICEState
	const sid = "cam-nilice"
	c.StartPublish(sid, signToken(t, sid, ts.hmacKey), ts.url(), nil)
	eventually(t, 5*time.Second, ts.gotPublish.Load,
		"publish should still complete with a nil OnICEState")
}
