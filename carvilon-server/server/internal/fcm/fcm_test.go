package fcm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/oauth2"
)

// senderTo builds a Sender pointed at a test endpoint with a static
// access token (no real Google OAuth2 round-trip).
func senderTo(url string) *Sender {
	return &Sender{
		httpClient: http.DefaultClient,
		ts:         oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-access-token"}),
		sendURL:    url,
	}
}

func TestSend_BuildsV1DataMessage(t *testing.T) {
	var gotAuth, gotCT string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name":"projects/p/messages/1"}`))
	}))
	defer srv.Close()

	push := DoorbellPush{
		StreamID:    "0c:ea:14:00:00:01",
		DeviceName:  "Hauseingang",
		RoomID:      "WR-room-1",
		EventID:     "42",
		CancelToken: "cancel-abc",
		TS:          "1716998400",
	}
	if err := senderTo(srv.URL).Send(context.Background(), "device-token-xyz", push); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if gotAuth != "Bearer test-access-token" {
		t.Errorf("Authorization = %q, want Bearer test-access-token", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}

	message, ok := gotBody["message"].(map[string]any)
	if !ok {
		t.Fatalf("body has no message object: %v", gotBody)
	}
	if message["token"] != "device-token-xyz" {
		t.Errorf("message.token = %v, want device-token-xyz", message["token"])
	}
	android, _ := message["android"].(map[string]any)
	if android["priority"] != "high" {
		t.Errorf("android.priority = %v, want high", android["priority"])
	}
	data, ok := message["data"].(map[string]any)
	if !ok {
		t.Fatalf("message has no data object: %v", message)
	}
	want := map[string]string{
		"type":         "doorbell_ring",
		"stream_id":    "0c:ea:14:00:00:01",
		"device_name":  "Hauseingang",
		"room_id":      "WR-room-1",
		"event_id":     "42",
		"cancel_token": "cancel-abc",
		"ts":           "1716998400",
	}
	for k, v := range want {
		if data[k] != v {
			t.Errorf("data[%q] = %v, want %q", k, data[k], v)
		}
	}
	// notification key must be absent (data-only message).
	if _, present := message["notification"]; present {
		t.Error("message.notification present; want data-only message")
	}
}

func TestSend_Non2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"status":"INVALID_ARGUMENT"}}`))
	}))
	defer srv.Close()

	err := senderTo(srv.URL).Send(context.Background(), "t", DoorbellPush{StreamID: "x"})
	if err == nil {
		t.Fatal("Send to a 400 endpoint returned nil error")
	}
}

func TestSendConfigChanged_BuildsDataOnlyMessage(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name":"projects/p/messages/3"}`))
	}))
	defer srv.Close()

	if err := senderTo(srv.URL).SendConfigChanged(context.Background(), "device-token-xyz", "0c:ea:14:00:00:09"); err != nil {
		t.Fatalf("SendConfigChanged: %v", err)
	}

	message, ok := gotBody["message"].(map[string]any)
	if !ok {
		t.Fatalf("body has no message object: %v", gotBody)
	}
	if message["token"] != "device-token-xyz" {
		t.Errorf("message.token = %v, want device-token-xyz", message["token"])
	}
	android, _ := message["android"].(map[string]any)
	if android["priority"] != "high" {
		t.Errorf("android.priority = %v, want high", android["priority"])
	}
	data, ok := message["data"].(map[string]any)
	if !ok {
		t.Fatalf("message has no data object: %v", message)
	}
	want := map[string]string{
		"type":       "config.changed",
		"viewer_mac": "0c:ea:14:00:00:09",
	}
	for k, v := range want {
		if data[k] != v {
			t.Errorf("data[%q] = %v, want %q", k, data[k], v)
		}
	}
	// Signal-only: no setting values, and none of the doorbell fields.
	if len(data) != len(want) {
		t.Errorf("data has %d keys (%v), want exactly %d (signal-only)", len(data), data, len(want))
	}
	// data-only: no notification block (so onMessageReceived fires in Doze).
	if _, present := message["notification"]; present {
		t.Error("message.notification present; want data-only message")
	}
}

func TestNewSender_EmptyPathDisabled(t *testing.T) {
	s, err := NewSender(context.Background(), "", "project-id")
	if err != nil {
		t.Fatalf("NewSender(empty) err = %v, want nil (disabled)", err)
	}
	if s != nil {
		t.Errorf("NewSender(empty) = %v, want nil sender (disabled)", s)
	}
}

func TestNewSender_MissingFileIsError(t *testing.T) {
	s, err := NewSender(context.Background(), "/no/such/service-account.json", "project-id")
	if err == nil {
		t.Fatal("NewSender(missing file) err = nil, want error")
	}
	if s != nil {
		t.Errorf("NewSender(missing file) = %v, want nil sender", s)
	}
}

func TestSendCancel_BuildsDoorbellCancelMessage(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name":"projects/p/messages/2"}`))
	}))
	defer srv.Close()

	push := DoorbellPush{
		StreamID:    "0c:ea:14:00:00:01",
		CancelToken: "cancel-abc",
		Reason:      "timeout",
		// ring-only fields are set but must be IGNORED by SendCancel:
		DeviceName: "Hauseingang",
		RoomID:     "WR-room-1",
		EventID:    "42",
		TS:         "1716998400",
	}
	if err := senderTo(srv.URL).SendCancel(context.Background(), "device-token-xyz", push); err != nil {
		t.Fatalf("SendCancel: %v", err)
	}

	message, ok := gotBody["message"].(map[string]any)
	if !ok {
		t.Fatalf("body has no message object: %v", gotBody)
	}
	if message["token"] != "device-token-xyz" {
		t.Errorf("message.token = %v, want device-token-xyz", message["token"])
	}
	android, _ := message["android"].(map[string]any)
	if android["priority"] != "high" {
		t.Errorf("android.priority = %v, want high", android["priority"])
	}
	data, ok := message["data"].(map[string]any)
	if !ok {
		t.Fatalf("message has no data object: %v", message)
	}
	want := map[string]string{
		"type":         "doorbell_cancel",
		"stream_id":    "0c:ea:14:00:00:01",
		"cancel_token": "cancel-abc",
		"reason":       "timeout",
	}
	for k, v := range want {
		if data[k] != v {
			t.Errorf("data[%q] = %v, want %q", k, data[k], v)
		}
	}
	// ring-only keys must NOT appear on a cancel push.
	for _, k := range []string{"device_name", "room_id", "event_id", "ts"} {
		if _, present := data[k]; present {
			t.Errorf("data[%q] present on doorbell_cancel; want absent", k)
		}
	}
	if _, present := message["notification"]; present {
		t.Error("message.notification present; want data-only message")
	}
}
