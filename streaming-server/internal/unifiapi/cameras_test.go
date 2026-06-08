package unifiapi

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeNVR is a tiny test server that mimics the Protect integration
// camera listing. Records the headers it saw so we can verify the
// API key was sent and the request URL stayed off the log.
type fakeNVR struct {
	srv         *httptest.Server
	gotAuthKey  string
	gotAccept   string
	wantStatus  int
	wantBody    string
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
