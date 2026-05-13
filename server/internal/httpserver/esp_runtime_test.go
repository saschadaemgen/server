package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
