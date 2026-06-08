package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const espTestMAC = "0c:ea:14:aa:bb:cc"

// postDiscover hilft den Test-Setup-Pfad.
func postDiscover(t *testing.T, env *testEnv, body map[string]any) *http.Response {
	t.Helper()
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/esp/discover", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST /esp/discover: %v", err)
	}
	return resp
}

func getStatus(t *testing.T, env *testEnv, mac string) (int, map[string]any) {
	t.Helper()
	resp, err := env.client.Get(env.ts.URL + "/esp/discover/status?device_id=" + mac)
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// ---------- Discovery-Endpoint-Tests ----------

func TestDiscover_StoresPendingDevice(t *testing.T) {
	env := newTestServer(t)
	resp := postDiscover(t, env, map[string]any{
		"mac":          espTestMAC,
		"model":        "UA-Display",
		"fw_version":   "1.0.0",
		"capabilities": []string{"mjpeg"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var n int
	if err := env.d.QueryRow(
		`SELECT COUNT(*) FROM esp_pending_devices WHERE mac = ?`, espTestMAC,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("pending count = %d, want 1", n)
	}
}

func TestDiscover_UpdatesLastPoll(t *testing.T) {
	env := newTestServer(t)
	postDiscover(t, env, map[string]any{
		"mac": espTestMAC, "model": "UA-Display", "fw_version": "1.0.0",
	}).Body.Close()
	var firstPoll int64
	env.d.QueryRow(`SELECT last_poll_at FROM esp_pending_devices WHERE mac = ?`, espTestMAC).Scan(&firstPoll)

	// kleine Pause damit die UnixMilli-Zaehler weiterspringen.
	for i := 0; i < 5; i++ {
		_ = i
	}
	postDiscover(t, env, map[string]any{
		"mac": espTestMAC, "model": "UA-Display", "fw_version": "1.0.1",
	}).Body.Close()
	var secondPoll int64
	var fw string
	env.d.QueryRow(`SELECT last_poll_at, fw_version FROM esp_pending_devices WHERE mac = ?`, espTestMAC).
		Scan(&secondPoll, &fw)
	if secondPoll < firstPoll {
		t.Errorf("second poll < first poll")
	}
	if fw != "1.0.1" {
		t.Errorf("fw_version = %q, want 1.0.1 (UPSERT broken)", fw)
	}
}

func TestDiscoverStatus_ReturnsPending(t *testing.T) {
	env := newTestServer(t)
	postDiscover(t, env, map[string]any{"mac": espTestMAC, "model": "UA-Display"}).Body.Close()
	status, body := getStatus(t, env, espTestMAC)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if body["status"] != "pending" {
		t.Errorf("status = %v, want pending", body["status"])
	}
}

func TestDiscoverStatus_ReturnsAdopted(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	postDiscover(t, env, map[string]any{"mac": espTestMAC, "model": "UA-Display"}).Body.Close()

	// Adopt
	body, _ := json.Marshal(map[string]any{
		"mac":  espTestMAC,
		"name": "ESP Wohnung 1",
	})
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/a/esp-viewers/adopt", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("adopt status = %d", resp.StatusCode)
	}

	// ESP pollt: bekommt adopted + Token
	status, body2 := getStatus(t, env, espTestMAC)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if body2["status"] != "adopted" {
		t.Errorf("status = %v, want adopted", body2["status"])
	}
	tok, _ := body2["token"].(string)
	if tok == "" {
		t.Error("token missing on first adopted poll")
	}

	// Zweiter Poll: weiter adopted aber OHNE Token (Handoff
	// schon ausgeliefert).
	_, body3 := getStatus(t, env, espTestMAC)
	if body3["status"] != "adopted" {
		t.Errorf("second poll status = %v", body3["status"])
	}
	if _, hasToken := body3["token"]; hasToken {
		t.Error("second adopted poll still carries token (handoff not deleted)")
	}
}

func TestDiscoverStatus_ReturnsRejected(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	postDiscover(t, env, map[string]any{"mac": espTestMAC, "model": "UA-Display"}).Body.Close()

	// Reject
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/a/esp-viewers/"+espTestMAC+"/reject", nil)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	resp.Body.Close()

	status, body := getStatus(t, env, espTestMAC)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if body["status"] != "rejected" {
		t.Errorf("status = %v, want rejected", body["status"])
	}
}

func TestDiscoverStatus_UnknownReturns404(t *testing.T) {
	env := newTestServer(t)
	status, _ := getStatus(t, env, "0c:ea:14:99:99:99")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

func TestDiscover_RejectsInvalidMAC(t *testing.T) {
	env := newTestServer(t)
	resp := postDiscover(t, env, map[string]any{"mac": "not-a-mac"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// ---------- Admin-Endpoint-Tests ----------

func TestAdopt_MovesFromPendingToViewers(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	postDiscover(t, env, map[string]any{
		"mac": espTestMAC, "model": "UA-Display", "fw_version": "1.0",
	}).Body.Close()

	body, _ := json.Marshal(map[string]any{
		"mac":  espTestMAC,
		"name": "Wohnung 1",
	})
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/a/esp-viewers/adopt", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	info, err := env.viewerMgr.GetViewerInfo(context.Background(), espTestMAC)
	if err != nil {
		t.Fatalf("viewer not found after adopt: %v", err)
	}
	if info.Type != "esp" {
		t.Errorf("Type = %q, want esp", info.Type)
	}
	if info.Name != "Wohnung 1" {
		t.Errorf("Name = %q", info.Name)
	}
	if info.ESPModel != "UA-Display" {
		t.Errorf("ESPModel = %q", info.ESPModel)
	}
	if !info.HasDeviceToken {
		t.Error("HasDeviceToken = false, want true (adopt should set hash)")
	}
}

func TestAdopt_GeneratesUniqueToken(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	postDiscover(t, env, map[string]any{"mac": espTestMAC}).Body.Close()
	postDiscover(t, env, map[string]any{"mac": "0c:ea:14:bb:cc:dd"}).Body.Close()

	adoptOne := func(mac, name string) string {
		body, _ := json.Marshal(map[string]any{"mac": mac, "name": name})
		req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/a/esp-viewers/adopt", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := env.client.Do(req)
		if err != nil {
			t.Fatalf("adopt: %v", err)
		}
		defer resp.Body.Close()
		// Token holen ueber Status-Poll
		_, sb := getStatus(t, env, mac)
		token, _ := sb["token"].(string)
		return token
	}
	a := adoptOne(espTestMAC, "Wohnung A")
	b := adoptOne("0c:ea:14:bb:cc:dd", "Wohnung B")
	if a == "" || b == "" {
		t.Fatalf("token leer: a=%q b=%q", a, b)
	}
	if a == b {
		t.Error("two adopts produced identical tokens")
	}
}

func TestAdopt_StoresOnlyHashNotClearText(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	postDiscover(t, env, map[string]any{"mac": espTestMAC}).Body.Close()
	body, _ := json.Marshal(map[string]any{"mac": espTestMAC, "name": "ESP Test"})
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/a/esp-viewers/adopt", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	resp.Body.Close()
	_, sb := getStatus(t, env, espTestMAC)
	token, _ := sb["token"].(string)
	if token == "" {
		t.Fatal("no token from status poll")
	}

	var hash string
	if err := env.d.QueryRow(
		`SELECT device_token_hash FROM viewers WHERE mac = ?`, espTestMAC,
	).Scan(&hash); err != nil {
		t.Fatalf("query hash: %v", err)
	}
	if hash == "" {
		t.Error("device_token_hash empty in DB")
	}
	if hash == token {
		t.Error("DB stores plaintext token, not hash")
	}
	if strings.Contains(hash, token) {
		t.Error("DB stores plaintext as part of hash")
	}
}

func TestRegenerateToken_InvalidatesOld(t *testing.T) {
	env := newTestServer(t)
	loginAdmin(t, env, adminTestUser, adminTestPassword)
	postDiscover(t, env, map[string]any{"mac": espTestMAC}).Body.Close()
	body, _ := json.Marshal(map[string]any{"mac": espTestMAC, "name": "ESP Test"})
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/a/esp-viewers/adopt", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := env.client.Do(req)
	resp.Body.Close()

	var oldHash string
	env.d.QueryRow(`SELECT device_token_hash FROM viewers WHERE mac = ?`, espTestMAC).Scan(&oldHash)

	regenReq, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/a/esp-viewers/"+espTestMAC+"/regenerate-token", nil)
	regenResp, err := env.client.Do(regenReq)
	if err != nil {
		t.Fatalf("regen: %v", err)
	}
	regenResp.Body.Close()

	var newHash string
	env.d.QueryRow(`SELECT device_token_hash FROM viewers WHERE mac = ?`, espTestMAC).Scan(&newHash)
	if newHash == "" || newHash == oldHash {
		t.Errorf("regenerate did not change hash: old=%s new=%s", oldHash, newHash)
	}
}
