// Package fcm sends doorbell push notifications to the native viewer
// apps via the Firebase Cloud Messaging HTTP v1 API. carvilon calls
// the FCM endpoint directly over HTTPS; it does NOT use the Firebase
// Admin SDK (too large a dependency, against the dependency doctrine).
//
// The only third-party dependency is golang.org/x/oauth2/google, used
// to mint a short-lived OAuth2 access token from a service-account
// JSON (scope firebase.messaging). The TokenSource refreshes itself
// before expiry, so Send just reads the current token.
//
// This is a pure cloud path: it runs on the edge (RPi) but reaches out
// to Google. A failure (no internet, bad token, FCM error) is the
// caller's to log and swallow; the local doorbell flow must not depend
// on it.
package fcm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// messagingScope is the OAuth2 scope for the FCM v1 send API.
const messagingScope = "https://www.googleapis.com/auth/firebase.messaging"

// httpTimeout caps a single send round-trip.
const httpTimeout = 10 * time.Second

// DoorbellPush is the data carried in a doorbell_ring push. Every
// field becomes a string entry in the FCM message "data" map (FCM v1
// data values are strings only); the app reads them and builds its own
// ring UI.
type DoorbellPush struct {
	StreamID    string // viewer/mock MAC - the WHEP/stream correlation id
	DeviceName  string // resolved calling-intercom name, may be empty
	RoomID      string // WebRTC room id from the doorbell event
	EventID     string // door_events row id (decimal string), may be "0"
	CancelToken string // one-shot match key for the cancel push
	TS          string // event timestamp, unix seconds (decimal string)
}

// Sender posts doorbell pushes to FCM HTTP v1. Build with NewSender;
// it is safe for concurrent use (http.Client and the TokenSource are).
type Sender struct {
	httpClient *http.Client
	ts         oauth2.TokenSource
	sendURL    string // https://fcm.googleapis.com/v1/projects/<id>/messages:send
}

// NewSender builds a Sender from a service-account JSON file and the
// Firebase project id.
//
// Disabled handling: an EMPTY path returns (nil, nil) - FCM is simply
// off, and the caller (which already nil-checks the sender) skips the
// push leg. A NON-EMPTY path that cannot be read or parsed returns an
// error: that is a real misconfiguration the operator set and should
// see. The config layer guarantees both path and project id are set
// together, so a half-config never reaches here.
func NewSender(ctx context.Context, serviceAccountJSONPath, projectID string) (*Sender, error) {
	if serviceAccountJSONPath == "" {
		return nil, nil // disabled
	}
	data, err := os.ReadFile(serviceAccountJSONPath)
	if err != nil {
		return nil, fmt.Errorf("fcm: read service account json: %w", err)
	}
	creds, err := google.CredentialsFromJSON(ctx, data, messagingScope)
	if err != nil {
		return nil, fmt.Errorf("fcm: parse service account json: %w", err)
	}
	return &Sender{
		httpClient: &http.Client{Timeout: httpTimeout},
		ts:         creds.TokenSource,
		sendURL:    "https://fcm.googleapis.com/v1/projects/" + projectID + "/messages:send",
	}, nil
}

// Send posts a single doorbell push to the given device token. It
// returns an error on token-mint failure, transport failure, or any
// non-2xx FCM response (with the response body for diagnosis). The
// caller logs and swallows - a push failure never propagates into the
// doorbell flow.
func (s *Sender) Send(ctx context.Context, token string, push DoorbellPush) error {
	msg := map[string]any{
		"message": map[string]any{
			"token": token,
			"data": map[string]string{
				"type":         "doorbell_ring",
				"stream_id":    push.StreamID,
				"device_name":  push.DeviceName,
				"room_id":      push.RoomID,
				"event_id":     push.EventID,
				"cancel_token": push.CancelToken,
				"ts":           push.TS,
			},
			"android": map[string]any{
				"priority": "high",
			},
		},
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("fcm: marshal message: %w", err)
	}

	tok, err := s.ts.Token()
	if err != nil {
		return fmt.Errorf("fcm: mint access token: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.sendURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("fcm: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fcm: send: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("fcm: send returned %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}
