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
	// Reason is ONLY carried on a doorbell_cancel push (SendCancel): the
	// lifecycle cancel_reason ("timeout" for a UA abort today). The
	// doorbell_ring Send ignores it. (Saison 19-20)
	Reason string
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

// SendCancel posts a doorbell_cancel push, telling the app to close the ring
// overlay it opened from the matching doorbell_ring (Saison 19-20). It mirrors
// Send but with a SEPARATE, smaller data map - type "doorbell_cancel", the
// stream_id + cancel_token (the app matches the open ring by cancel_token), and
// the reason - so the doorbell_ring Send is left untouched. android.priority is
// high, same as the ring, so it arrives in the background. Same best-effort
// contract: the caller logs and swallows any error. The HTTP machinery is kept
// inline (not factored out of Send) so the proven ring path stays byte-for-byte
// unchanged.
func (s *Sender) SendCancel(ctx context.Context, token string, push DoorbellPush) error {
	msg := map[string]any{
		"message": map[string]any{
			"token": token,
			"data": map[string]string{
				"type":         "doorbell_cancel",
				"stream_id":    push.StreamID,
				"cancel_token": push.CancelToken,
				"reason":       push.Reason,
			},
			"android": map[string]any{
				"priority": "high",
			},
		},
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("fcm: marshal cancel message: %w", err)
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

// SendConfigChanged posts a data-only config.changed wake-up to one device
// token (Saison 19-34). It is the Android Doze transport for the existing
// signal+refetch model: SSE/eventbus is the foreground channel; this FCM push
// wakes a backgrounded app so it refetches /webviewer/settings. The payload is
// SIGNAL-ONLY - type + viewer_mac, NO setting values - mirroring the empty SSE
// config.changed frame so the app cannot drift on stale fields. data-only (no
// notification block) so onMessageReceived fires without a system-tray banner;
// android.priority high so it arrives in Doze. The HTTP machinery is inline
// (not factored out of Send) so the proven ring/cancel paths stay byte-for-byte
// unchanged. Same best-effort contract: the caller logs and swallows.
func (s *Sender) SendConfigChanged(ctx context.Context, token, viewerMAC string) error {
	msg := map[string]any{
		"message": map[string]any{
			"token": token,
			"data": map[string]string{
				"type":       "config.changed",
				"viewer_mac": viewerMAC,
			},
			"android": map[string]any{
				"priority": "high",
			},
		},
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("fcm: marshal config.changed message: %w", err)
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
