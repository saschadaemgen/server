package httpserver

import (
	"database/sql"
	"net/http"
	"strings"
	"testing"

	"carvilon.local/server/internal/auth/esptoken"
)

// seedAndroidViewer inserts an android viewers row directly into
// the test DB with a fresh bearer token and returns the cleartext
// token. Direct INSERT (not AddViewer) keeps the test free of the
// mock-goroutine spawn; the bearer middleware + SetFCMToken both
// read / write the row straight from the DB, so no in-memory
// cache entry is needed for the auth match.
func seedAndroidViewer(t *testing.T, env *testEnv, mac string, port int64) string {
	t.Helper()
	clear, hash, err := esptoken.Generate()
	if err != nil {
		t.Fatalf("esptoken.Generate: %v", err)
	}
	if _, err := env.d.Exec(
		`INSERT INTO viewers (mac, name, service_port, type, device_token_hash, created_at, updated_at)
		 VALUES (?, ?, ?, 'android', ?, 0, 0)`,
		mac, "android-"+mac, port, hash,
	); err != nil {
		t.Fatalf("seed android viewer: %v", err)
	}
	return clear
}

func fcmTokenInDB(t *testing.T, env *testEnv, mac string) sql.NullString {
	t.Helper()
	var got sql.NullString
	if err := env.d.QueryRow(
		`SELECT fcm_token FROM viewers WHERE mac = ?`, mac,
	).Scan(&got); err != nil {
		t.Fatalf("probe fcm_token: %v", err)
	}
	return got
}

func TestMieterFCMToken_SetsTokenWithBearer(t *testing.T) {
	env := newTestServer(t)
	const mac = "0c:ea:14:dd:ee:ff"
	token := seedAndroidViewer(t, env, mac, 8300)

	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/webviewer/fcm-token",
		strings.NewReader(`{"fcm_token":"fcm-abc-123"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := fcmTokenInDB(t, env, mac)
	if !got.Valid || got.String != "fcm-abc-123" {
		t.Errorf("fcm_token in DB = %+v, want fcm-abc-123", got)
	}
}

// A second POST with a different token must overwrite (token
// refresh path - Google rotates tokens; the app re-registers).
func TestMieterFCMToken_RefreshOverwrites(t *testing.T) {
	env := newTestServer(t)
	const mac = "0c:ea:14:dd:ee:ff"
	token := seedAndroidViewer(t, env, mac, 8300)

	post := func(body string) {
		req, _ := http.NewRequest(http.MethodPost,
			env.ts.URL+"/webviewer/fcm-token", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := env.client.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	}
	post(`{"fcm_token":"old-token"}`)
	post(`{"fcm_token":"new-token"}`)
	got := fcmTokenInDB(t, env, mac)
	if !got.Valid || got.String != "new-token" {
		t.Errorf("fcm_token = %+v, want new-token (refresh overwrite)", got)
	}
}

func TestMieterFCMToken_RejectsInvalidBearer(t *testing.T) {
	env := newTestServer(t)
	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/webviewer/fcm-token",
		strings.NewReader(`{"fcm_token":"x"}`))
	req.Header.Set("Authorization", "Bearer bogus-not-a-real-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for invalid bearer", resp.StatusCode)
	}
}

func TestMieterFCMToken_RejectsEmptyToken(t *testing.T) {
	env := newTestServer(t)
	const mac = "0c:ea:14:dd:ee:ff"
	token := seedAndroidViewer(t, env, mac, 8300)

	req, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/webviewer/fcm-token",
		strings.NewReader(`{"fcm_token":"   "}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for empty token", resp.StatusCode)
	}
	// The row's token column stays NULL - nothing was written.
	if got := fcmTokenInDB(t, env, mac); got.Valid {
		t.Errorf("fcm_token = %q, want NULL after rejected empty post", got.String)
	}
}

func TestMieterFCMTokenDelete_ClearsToken(t *testing.T) {
	env := newTestServer(t)
	const mac = "0c:ea:14:dd:ee:ff"
	token := seedAndroidViewer(t, env, mac, 8300)

	// Set a token first.
	setReq, _ := http.NewRequest(http.MethodPost,
		env.ts.URL+"/webviewer/fcm-token",
		strings.NewReader(`{"fcm_token":"to-be-cleared"}`))
	setReq.Header.Set("Authorization", "Bearer "+token)
	setReq.Header.Set("Content-Type", "application/json")
	setResp, err := env.client.Do(setReq)
	if err != nil {
		t.Fatalf("POST set: %v", err)
	}
	setResp.Body.Close()
	if got := fcmTokenInDB(t, env, mac); !got.Valid || got.String != "to-be-cleared" {
		t.Fatalf("setup failed: fcm_token = %+v", got)
	}

	// Now DELETE clears it.
	delReq, _ := http.NewRequest(http.MethodDelete,
		env.ts.URL+"/webviewer/fcm-token", nil)
	delReq.Header.Set("Authorization", "Bearer "+token)
	delResp, err := env.client.Do(delReq)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200", delResp.StatusCode)
	}
	if got := fcmTokenInDB(t, env, mac); got.Valid {
		t.Errorf("fcm_token = %q after DELETE, want NULL", got.String)
	}
}
