package unifiapi

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeNVR is a tiny test server that mimics the Protect integration
// camera listing. Records the headers it saw so we can verify the
// API key was sent and the request URL stayed off the log.
type fakeNVR struct {
	srv        *httptest.Server
	gotAuthKey string
	gotAccept  string
	wantStatus int
	wantBody   string
}

func newFakeNVR(t *testing.T, body string, status int) *fakeNVR {
	t.Helper()
	f := &fakeNVR{wantStatus: status, wantBody: body}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/proxy/protect/integration/v1/cameras" {
			http.Error(w, "wrong path: "+r.URL.Path, http.StatusNotFound)
			return
		}
		f.gotAuthKey = r.Header.Get("X-API-KEY")
		f.gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// clientForFake builds a Client wired at the fakeNVR. The HTTP client
// has TLS skip-verify because the test server's cert isn't trusted.
func clientForFake(t *testing.T, f *fakeNVR, apiKey string) *Client {
	t.Helper()
	hc := f.srv.Client()
	hc.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
	}
	host := strings.TrimPrefix(f.srv.URL, "https://")
	c, err := New(Options{
		NVRHost:    host,
		APIKey:     apiKey,
		HTTPClient: hc,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestListCameras_DecodesAndMaps(t *testing.T) {
	body := `[
		{"id":"abc","name":"Intercom","state":"CONNECTED","hasPackageCamera":true},
		{"id":"def","name":"AI 360","state":"DISCONNECTED","hasPackageCamera":false},
		{"id":"ghi","name":"G3 Flex","state":"CONNECTED","hasPackageCamera":false}
	]`
	f := newFakeNVR(t, body, http.StatusOK)
	c := clientForFake(t, f, "secret-key")

	cams, err := c.ListCameras(context.Background())
	if err != nil {
		t.Fatalf("ListCameras: %v", err)
	}
	if len(cams) != 3 {
		t.Fatalf("got %d cameras, want 3", len(cams))
	}

	got := map[string]Camera{}
	for _, c := range cams {
		got[c.ID] = c
	}

	if !got["abc"].Online {
		t.Errorf("abc should be Online")
	}
	if !got["abc"].HasPackageCam {
		t.Errorf("abc should HasPackageCam")
	}
	if got["def"].Online {
		t.Errorf("def (DISCONNECTED) should NOT be Online")
	}
	if got["abc"].Name != "Intercom" {
		t.Errorf("abc.Name = %q, want Intercom", got["abc"].Name)
	}
}

func TestListCameras_SendsAPIKey(t *testing.T) {
	f := newFakeNVR(t, `[]`, http.StatusOK)
	c := clientForFake(t, f, "the-secret-key")
	if _, err := c.ListCameras(context.Background()); err != nil {
		t.Fatalf("ListCameras: %v", err)
	}
	if f.gotAuthKey != "the-secret-key" {
		t.Errorf("X-API-KEY = %q, want %q", f.gotAuthKey, "the-secret-key")
	}
}

func TestListCameras_ErrorOnNon2xx(t *testing.T) {
	f := newFakeNVR(t, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	c := clientForFake(t, f, "wrong-key")
	_, err := c.ListCameras(context.Background())
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error %q should mention status 401", err.Error())
	}
}

func TestListCameras_ErrorDoesNotLeakKey(t *testing.T) {
	// Even on error the request URL must not be quoted (it contains
	// the host) and the headers certainly must not be.
	f := newFakeNVR(t, `nope`, http.StatusForbidden)
	c := clientForFake(t, f, "secret-must-never-leak")
	_, err := c.ListCameras(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "secret-must-never-leak") {
		t.Errorf("API key leaked into error: %v", err)
	}
	if strings.Contains(err.Error(), "X-API-KEY") {
		t.Errorf("header name leaked into error: %v", err)
	}
}

func TestListCameras_EmptyArray(t *testing.T) {
	f := newFakeNVR(t, `[]`, http.StatusOK)
	c := clientForFake(t, f, "k")
	cams, err := c.ListCameras(context.Background())
	if err != nil {
		t.Fatalf("ListCameras: %v", err)
	}
	if len(cams) != 0 {
		t.Errorf("got %d cameras, want 0", len(cams))
	}
}

func TestListCameras_MalformedJSONReturnsError(t *testing.T) {
	f := newFakeNVR(t, `{"not":"an array"}`, http.StatusOK)
	c := clientForFake(t, f, "k")
	_, err := c.ListCameras(context.Background())
	if err == nil {
		t.Fatal("expected decode error")
	}
}

func TestNew_RejectsMissingFields(t *testing.T) {
	if _, err := New(Options{NVRHost: "", APIKey: "k"}); err == nil {
		t.Error("expected error for empty NVRHost")
	}
	if _, err := New(Options{NVRHost: "h", APIKey: ""}); err == nil {
		t.Error("expected error for empty APIKey")
	}
}

func TestListCameras_ContextCancelled(t *testing.T) {
	// If the caller's context is already cancelled, the request must
	// not be sent.
	f := newFakeNVR(t, `[]`, http.StatusOK)
	c := clientForFake(t, f, "k")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.ListCameras(ctx)
	if err == nil {
		t.Fatal("expected context-cancelled error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled chain", err)
	}
}

// Verify the JSON tags survive a marshal round-trip — proves the wire
// shape stays compatible with carvilon-server/streams.Camera.
func TestCamera_JSONTags(t *testing.T) {
	in := Camera{ID: "x", Name: "n", Online: true, HasPackageCam: true}
	buf, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, want := range []string{`"id":"x"`, `"name":"n"`, `"online":true`, `"has_package_cam":true`} {
		if !strings.Contains(string(buf), want) {
			t.Errorf("JSON missing %q: %s", want, buf)
		}
	}
}

// --- RTSPS stream management tests (S20-E1) ---------------------------------

// fakeRTSPS mimics the Protect rtsps-stream endpoint for ONE camera. It
// records the method, API key and request body it saw so the tests can
// verify the wire contract and that the key is never echoed into errors.
type fakeRTSPS struct {
	srv        *httptest.Server
	gotMethod  string
	gotAuthKey string
	gotBody    string
}

func newFakeRTSPS(t *testing.T, cameraID, body string, status int) *fakeRTSPS {
	t.Helper()
	f := &fakeRTSPS{}
	want := "/proxy/protect/integration/v1/cameras/" + cameraID + "/rtsps-stream"
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != want {
			http.Error(w, "wrong path: "+r.URL.Path, http.StatusNotFound)
			return
		}
		f.gotMethod = r.Method
		f.gotAuthKey = r.Header.Get("X-API-KEY")
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			f.gotBody = string(b)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func clientForRTSPS(t *testing.T, f *fakeRTSPS, apiKey string) *Client {
	t.Helper()
	hc := f.srv.Client()
	hc.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
	}
	host := strings.TrimPrefix(f.srv.URL, "https://")
	c, err := New(Options{NVRHost: host, APIKey: apiKey, HTTPClient: hc})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestGetRTSPSStream_DecodesURLs(t *testing.T) {
	body := `{"high":"rtsps://h","medium":"","low":"rtsps://l"}`
	f := newFakeRTSPS(t, "cam1", body, http.StatusOK)
	c := clientForRTSPS(t, f, "the-secret-key")

	got, err := c.GetRTSPSStream(context.Background(), "cam1")
	if err != nil {
		t.Fatalf("GetRTSPSStream: %v", err)
	}
	if f.gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", f.gotMethod)
	}
	if f.gotAuthKey != "the-secret-key" {
		t.Errorf("X-API-KEY = %q, want the-secret-key", f.gotAuthKey)
	}
	if got.High != "rtsps://h" || got.Low != "rtsps://l" {
		t.Errorf("High/Low = %q/%q, want rtsps://h / rtsps://l", got.High, got.Low)
	}
	if got.Medium != "" || got.Package != "" {
		t.Errorf("Medium/Package = %q/%q, want empty", got.Medium, got.Package)
	}
}

func TestGetRTSPSStream_RequiresCameraID(t *testing.T) {
	f := newFakeRTSPS(t, "cam1", `{}`, http.StatusOK)
	c := clientForRTSPS(t, f, "k")
	if _, err := c.GetRTSPSStream(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty cameraID")
	}
	if f.gotMethod != "" {
		t.Errorf("server contacted (method=%q) despite empty cameraID", f.gotMethod)
	}
}

func TestCreateRTSPSStream_SendsQualitiesAndDecodes(t *testing.T) {
	f := newFakeRTSPS(t, "cam1", `{"high":"rtsps://h"}`, http.StatusOK)
	c := clientForRTSPS(t, f, "k")

	got, err := c.CreateRTSPSStream(context.Background(), "cam1", []string{"high"})
	if err != nil {
		t.Fatalf("CreateRTSPSStream: %v", err)
	}
	if f.gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", f.gotMethod)
	}
	if !strings.Contains(f.gotBody, `"qualities":["high"]`) {
		t.Errorf("body = %q, want it to carry qualities:[high]", f.gotBody)
	}
	if got.High != "rtsps://h" {
		t.Errorf("High = %q, want rtsps://h", got.High)
	}
}

func TestCreateRTSPSStream_RejectsInvalidInput(t *testing.T) {
	f := newFakeRTSPS(t, "cam1", `{}`, http.StatusOK)
	c := clientForRTSPS(t, f, "k")

	if _, err := c.CreateRTSPSStream(context.Background(), "cam1", []string{"ultra"}); err == nil {
		t.Error("expected error for invalid quality")
	}
	if _, err := c.CreateRTSPSStream(context.Background(), "cam1", nil); err == nil {
		t.Error("expected error for empty qualities")
	}
	if _, err := c.CreateRTSPSStream(context.Background(), "", []string{"high"}); err == nil {
		t.Error("expected error for empty cameraID")
	}
	if f.gotMethod != "" {
		t.Errorf("server contacted (method=%q) despite invalid input; validation must precede the request", f.gotMethod)
	}
}

func TestDeleteRTSPSStream_SendsDelete(t *testing.T) {
	f := newFakeRTSPS(t, "cam1", "", http.StatusNoContent)
	c := clientForRTSPS(t, f, "the-secret-key")

	if err := c.DeleteRTSPSStream(context.Background(), "cam1"); err != nil {
		t.Fatalf("DeleteRTSPSStream: %v", err)
	}
	if f.gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", f.gotMethod)
	}
	if f.gotAuthKey != "the-secret-key" {
		t.Errorf("X-API-KEY = %q, want the-secret-key", f.gotAuthKey)
	}
}

func TestRTSPSStream_ErrorOnNon2xx(t *testing.T) {
	f := newFakeRTSPS(t, "cam1", `{"error":"unauthorized"}`, http.StatusUnauthorized)
	c := clientForRTSPS(t, f, "wrong-key")
	_, err := c.GetRTSPSStream(context.Background(), "cam1")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want one mentioning 401", err)
	}
}

func TestRTSPSStream_ErrorDoesNotLeakKey(t *testing.T) {
	f := newFakeRTSPS(t, "cam1", `nope`, http.StatusForbidden)
	c := clientForRTSPS(t, f, "secret-must-never-leak")
	_, err := c.GetRTSPSStream(context.Background(), "cam1")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "secret-must-never-leak") {
		t.Errorf("API key leaked into error: %v", err)
	}
	if strings.Contains(err.Error(), "X-API-KEY") {
		t.Errorf("header name leaked into error: %v", err)
	}
}
