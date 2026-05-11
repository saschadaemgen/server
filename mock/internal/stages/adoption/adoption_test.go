package adoption

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"unifix.local/mock/internal/identity"
	"unifix.local/mock/internal/state"
)

type silentLogger struct{}

func (silentLogger) Infof(string, ...any)  {}
func (silentLogger) Warnf(string, ...any)  {}
func (silentLogger) Errorf(string, ...any) {}

func newTestServer(t *testing.T) (*Server, *state.Store) {
	t.Helper()
	mac, err := net.ParseMAC("0c:ea:14:42:42:42")
	if err != nil {
		t.Fatalf("parse mac: %v", err)
	}
	id, err := identity.NewMockIdentity(mac, "", "2f840033-e0ce-4cf0-971a-25e61c275d07",
		net.ParseIP("192.168.1.42").To4(), 8080)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	store, err := state.New(t.TempDir())
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	certDir := filepath.Join(t.TempDir(), "certs")
	srv, err := New(id, store, silentLogger{}, ":0", certDir)
	if err != nil {
		t.Fatalf("adoption.New: %v", err)
	}
	return srv, store
}

func fakeBundleJSON(t *testing.T, mutate func(m map[string]any)) []byte {
	t.Helper()
	m := map[string]any{
		"broker_address":  "tls://192.168.1.1:12812",
		"broker_cert":     "-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----",
		"broker_cert_ca":  "-----BEGIN CERTIFICATE-----\nfakeca\n-----END CERTIFICATE-----",
		"broker_priv_key": "-----BEGIN EC PRIVATE KEY-----\nfakek\n-----END EC PRIVATE KEY-----",
		"controller_id":   "0cea14122cfd",
		"controller_type": "ULP-Go",
		"extras": map[string]any{
			"door_id":               "11111111-2222-3333-4444-555555555555",
			"door_name":             "UDM SE",
			"http_cert_fingerprint": "abc",
			"mqtt_cert_fingerprint": "def",
			"nacl_priv_key":         "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
			"nacl_pub_key":          "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=",
		},
		"name":         "UA Intercom Viewer 4242",
		"ssh_password": "test-pw",
		"ssh_user":     "ubnt",
	}
	if mutate != nil {
		mutate(m)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func doPOST(t *testing.T, srv *Server, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, adoptPath, bytes.NewReader(body))
	req.RemoteAddr = "192.168.1.1:54321"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestHandleAdopt_ValidBundle(t *testing.T) {
	srv, store := newTestServer(t)
	rec := doPOST(t, srv, fakeBundleJSON(t, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp["code"].(float64) != 0 {
		t.Errorf("code = %v, want 0", resp["code"])
	}
	if resp["msg"] != "success" {
		t.Errorf("msg = %v, want success", resp["msg"])
	}
	data := resp["data"].(map[string]any)
	if data["id"] != "0cea14424242" {
		t.Errorf("data.id = %v, want 0cea14424242", data["id"])
	}

	loaded, err := store.LoadBundle("0cea14424242")
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}
	if loaded == nil {
		t.Fatal("bundle not persisted")
	}
	complete, err := store.BundleComplete("0cea14424242")
	if err != nil {
		t.Fatalf("BundleComplete: %v", err)
	}
	if !complete {
		t.Error("BundleComplete = false after successful adopt")
	}
}

func TestHandleAdopt_MissingBrokerCert(t *testing.T) {
	srv, store := newTestServer(t)
	body := fakeBundleJSON(t, func(m map[string]any) { m["broker_cert"] = "" })
	rec := doPOST(t, srv, body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if got, _ := store.LoadBundle("0cea14424242"); got != nil {
		t.Error("bundle was persisted despite validation failure")
	}
}

func TestHandleAdopt_MissingBrokerCA(t *testing.T) {
	srv, _ := newTestServer(t)
	body := fakeBundleJSON(t, func(m map[string]any) { m["broker_cert_ca"] = "" })
	rec := doPOST(t, srv, body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleAdopt_MissingBrokerPrivKey(t *testing.T) {
	srv, _ := newTestServer(t)
	body := fakeBundleJSON(t, func(m map[string]any) { m["broker_priv_key"] = "" })
	rec := doPOST(t, srv, body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleAdopt_WrongBrokerAddressScheme(t *testing.T) {
	srv, _ := newTestServer(t)
	body := fakeBundleJSON(t, func(m map[string]any) {
		m["broker_address"] = "ssl://192.168.1.1:12812"
	})
	rec := doPOST(t, srv, body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleAdopt_MissingExtrasDoorID(t *testing.T) {
	srv, _ := newTestServer(t)
	body := fakeBundleJSON(t, func(m map[string]any) {
		m["extras"].(map[string]any)["door_id"] = ""
	})
	rec := doPOST(t, srv, body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleAdopt_NotPOST_GET(t *testing.T) {
	srv, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, adoptPath, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestHandleAdopt_NotPOST_PUT(t *testing.T) {
	srv, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPut, adoptPath, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestHandleAdopt_WrongPath(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, p := range []string{"/api/v1/adopt", "/adopt", "/"} {
		req := httptest.NewRequest(http.MethodPost, p, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("path %s: status = %d, want 404", p, rec.Code)
		}
	}
}

func TestHandleAdopt_ResponseFormat(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := doPOST(t, srv, fakeBundleJSON(t, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var resp adoptResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Data.Attrs.Adopted != "true" {
		t.Errorf("attrs.adopted = %q, want string \"true\"", resp.Data.Attrs.Adopted)
	}
	if !strings.HasPrefix(resp.Data.Attrs.Broker, "ssl://") {
		t.Errorf("attrs.broker = %q, want ssl:// prefix", resp.Data.Attrs.Broker)
	}
	if strings.Contains(resp.Data.Attrs.Broker, "tls://") {
		t.Errorf("attrs.broker still contains tls://, got %q", resp.Data.Attrs.Broker)
	}
	if resp.Data.Attrs.IPv4 != "192.168.1.42" {
		t.Errorf("attrs.ipv4 = %q, want 192.168.1.42", resp.Data.Attrs.IPv4)
	}
	if resp.Data.Attrs.MAC != "0c:ea:14:42:42:42" {
		t.Errorf("attrs.mac = %q, want 0c:ea:14:42:42:42", resp.Data.Attrs.MAC)
	}
}

func TestHandleAdopt_FiresAdoptedChan(t *testing.T) {
	srv, _ := newTestServer(t)
	select {
	case <-srv.AdoptedChan():
		t.Fatal("AdoptedChan was already closed before adoption")
	default:
	}
	rec := doPOST(t, srv, fakeBundleJSON(t, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	select {
	case <-srv.AdoptedChan():
		// good
	case <-time.After(time.Second):
		t.Fatal("AdoptedChan did not close after successful adoption")
	}
}

func TestHandleAdopt_BodyTooLarge(t *testing.T) {
	srv, _ := newTestServer(t)
	big := bytes.Repeat([]byte("x"), maxBodyBytes+1)
	rec := doPOST(t, srv, big)
	if rec.Code != http.StatusRequestEntityTooLarge && rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 413 or 400", rec.Code)
	}
}

func TestHandleAdopt_MalformedJSON(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := doPOST(t, srv, []byte("not json"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleAdopt_RejectsEmptyBody(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := doPOST(t, srv, []byte{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for empty body", rec.Code)
	}
}

// fakeUnlocker captures the args of the most recent Unlock call
// for assertion in HTTP-handler tests.
type fakeUnlocker struct {
	hubMAC      string
	intercomMAC string
	bellID      string
	requestID   string
	err         error
	called      bool
}

func (f *fakeUnlocker) Unlock(hubMAC, intercomMAC, bellID string) (string, error) {
	f.called = true
	f.hubMAC = hubMAC
	f.intercomMAC = intercomMAC
	f.bellID = bellID
	if f.err != nil {
		return "", f.err
	}
	if f.requestID == "" {
		f.requestID = "ZZZZZ"
	}
	return f.requestID, nil
}

func doUnlockPOST(t *testing.T, srv *Server, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, unlockPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestHandleUnlock_ValidRequest(t *testing.T) {
	srv, _ := newTestServer(t)
	fake := &fakeUnlocker{requestID: "AbCdE"}
	srv.SetUnlocker(fake)

	body := []byte(`{"hub_mac":"0cea14476781","intercom_mac":"28704e31e29c","bell_id":"bell0"}`)
	rec := doUnlockPOST(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp unlockResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.OK {
		t.Errorf("ok = false, want true")
	}
	if resp.RequestID != "AbCdE" {
		t.Errorf("request_id = %q, want AbCdE", resp.RequestID)
	}

	if !fake.called {
		t.Fatal("unlocker was not invoked")
	}
	if fake.hubMAC != "0cea14476781" {
		t.Errorf("hubMAC = %q", fake.hubMAC)
	}
	if fake.intercomMAC != "28704e31e29c" {
		t.Errorf("intercomMAC = %q", fake.intercomMAC)
	}
	if fake.bellID != "bell0" {
		t.Errorf("bellID = %q", fake.bellID)
	}
}

func TestHandleUnlock_MalformedJSON(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.SetUnlocker(&fakeUnlocker{})
	rec := doUnlockPOST(t, srv, []byte("not json"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	var resp unlockResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.OK {
		t.Error("ok = true, want false on malformed json")
	}
	if resp.Error == "" {
		t.Error("error string should be set")
	}
}

func TestHandleUnlock_MissingRequiredFields(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.SetUnlocker(&fakeUnlocker{})
	body := []byte(`{"bell_id":"bell0"}`)
	rec := doUnlockPOST(t, srv, body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for missing hub_mac/intercom_mac", rec.Code)
	}
}

func TestHandleUnlock_NoUnlockerReturns503(t *testing.T) {
	srv, _ := newTestServer(t)
	// Do NOT call SetUnlocker: MQTT is not yet wired.
	body := []byte(`{"hub_mac":"0cea14476781","intercom_mac":"28704e31e29c","bell_id":"bell0"}`)
	rec := doUnlockPOST(t, srv, body)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when no unlocker bound", rec.Code)
	}
	var resp unlockResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.OK {
		t.Error("ok = true, want false")
	}
	if resp.Error != "mqtt not connected" {
		t.Errorf("error = %q, want %q", resp.Error, "mqtt not connected")
	}
}

func TestHandleUnlock_RejectsNonPOST(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.SetUnlocker(&fakeUnlocker{})
	req := httptest.NewRequest(http.MethodGet, unlockPath, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

// Sanity check: io is imported for completeness if tests need to
// stream bodies in the future.
var _ = io.Discard
