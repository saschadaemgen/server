package shellyapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// recordingServer captures every JSON-RPC request so a test can assert the
// exact method + params the write path sends. Each method answers "{}".
type recordingServer struct {
	ts   *httptest.Server
	mu   sync.Mutex
	reqs []rpcRequest
}

type rpcRequest struct {
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

func newRecordingServer(t *testing.T, result string) *recordingServer {
	t.Helper()
	rs := &recordingServer{}
	rs.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		rs.mu.Lock()
		rs.reqs = append(rs.reqs, req)
		rs.mu.Unlock()
		if result == "" {
			result = "{}"
		}
		_, _ = w.Write([]byte(rpcResult(result)))
	}))
	t.Cleanup(rs.ts.Close)
	return rs
}

func (rs *recordingServer) last() rpcRequest {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.reqs[len(rs.reqs)-1]
}

func (rs *recordingServer) client() *Client {
	return New(Options{Address: strings.TrimPrefix(rs.ts.URL, "http://")})
}

func TestSetMQTTConfig(t *testing.T) {
	rs := newRecordingServer(t, `{"restart_required":true}`)
	restart, err := rs.client().SetMQTTConfig(context.Background(), MQTTProvision{
		Server: "192.168.1.2:8883", ClientID: "shelly-abc", User: "shelly-abc",
		Pass: "secret", SSLCA: "user_ca.pem", TopicPrefix: "carvilon/shelly-abc",
	})
	if err != nil {
		t.Fatalf("SetMQTTConfig: %v", err)
	}
	if !restart {
		t.Fatal("restart_required not propagated")
	}
	req := rs.last()
	if req.Method != "MQTT.SetConfig" {
		t.Fatalf("method = %q", req.Method)
	}
	cfg, _ := req.Params["config"].(map[string]any)
	if cfg == nil {
		t.Fatalf("no config in params: %+v", req.Params)
	}
	want := map[string]any{
		"enable": true, "server": "192.168.1.2:8883", "user": "shelly-abc",
		"pass": "secret", "ssl_ca": "user_ca.pem", "topic_prefix": "carvilon/shelly-abc",
		"client_id": "shelly-abc", "status_ntf": true, "rpc_ntf": true,
		"enable_rpc": true, "enable_control": true,
	}
	for k, v := range want {
		if cfg[k] != v {
			t.Errorf("config[%q] = %v, want %v", k, cfg[k], v)
		}
	}
}

func TestPutUserCA(t *testing.T) {
	rs := newRecordingServer(t, "{}")
	pem := "-----BEGIN CERTIFICATE-----\nMII...\n-----END CERTIFICATE-----\n"
	if err := rs.client().PutUserCA(context.Background(), pem); err != nil {
		t.Fatalf("PutUserCA: %v", err)
	}
	req := rs.last()
	if req.Method != "Shelly.PutUserCA" {
		t.Fatalf("method = %q", req.Method)
	}
	if req.Params["data"] != pem || req.Params["append"] != false {
		t.Fatalf("params = %+v", req.Params)
	}
	// Empty PEM clears the slot (data=null).
	if err := rs.client().PutUserCA(context.Background(), "  "); err != nil {
		t.Fatalf("PutUserCA clear: %v", err)
	}
	if d, ok := rs.last().Params["data"]; !ok || d != nil {
		t.Fatalf("clear should send data=null, got %v", d)
	}
}

func TestSetAuthHA1(t *testing.T) {
	rs := newRecordingServer(t, "{}")
	dev, pw := "shellypro4pm-08f9e0e5c790", "s3cret-pw"
	if err := rs.client().SetAuth(context.Background(), dev, pw); err != nil {
		t.Fatalf("SetAuth: %v", err)
	}
	req := rs.last()
	if req.Method != "Shelly.SetAuth" || req.Params["user"] != "admin" || req.Params["realm"] != dev {
		t.Fatalf("params = %+v", req.Params)
	}
	sum := sha256.Sum256([]byte("admin:" + dev + ":" + pw))
	if req.Params["ha1"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("ha1 = %v, want %s", req.Params["ha1"], hex.EncodeToString(sum[:]))
	}
	// Empty password clears auth (ha1=null).
	if err := rs.client().SetAuth(context.Background(), dev, ""); err != nil {
		t.Fatalf("SetAuth clear: %v", err)
	}
	if h, ok := rs.last().Params["ha1"]; !ok || h != nil {
		t.Fatalf("clear should send ha1=null, got %v", h)
	}
}

func TestSetCloudEnabled(t *testing.T) {
	rs := newRecordingServer(t, "{}")
	if _, err := rs.client().SetCloudEnabled(context.Background(), false); err != nil {
		t.Fatalf("SetCloudEnabled: %v", err)
	}
	req := rs.last()
	cfg, _ := req.Params["config"].(map[string]any)
	if req.Method != "Cloud.SetConfig" || cfg == nil || cfg["enable"] != false {
		t.Fatalf("params = %+v", req.Params)
	}
}
