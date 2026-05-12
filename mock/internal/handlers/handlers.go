// Package handlers implements method-specific RPC handlers that
// persist parsed body data into the mock's per-device state dir.
//
// Saison 11 scope: /update_tokens, /update_configs, /remote_view,
// /cancel_doorbell_notification. All other methods fall through
// to the mqtt-package DefaultHandler (best-effort success).
package handlers

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"unifix.local/mock/internal/state"
	"unifix.local/shared/proto"
)

// Logger is the minimal logging surface this package needs.
// Matches the shape used in the mqtt package so the same
// simpleLogger satisfies both.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

// Handler carries the dependencies every method handler needs.
// Constructed once at mock startup and passed by pointer.
//
// The OnDoorbell and OnDoorbellCancel callbacks are optional
// hooks for library consumers (mock.Viewer) that need to mirror
// doorbell pushes onto an event channel. Both callbacks run on
// the RPC handler goroutine and must not block; library code
// uses non-blocking channel sends.
type Handler struct {
	Store  *state.Store
	MockID string
	Log    Logger

	OnDoorbell       func(rec DoorbellRecord, rawBody []byte)
	OnDoorbellCancel func(rec DoorbellRecord, rawBody []byte)
}

// JWTRecord is the persisted shape of jwt.json. ReceivedAt is
// always emitted; the remaining fields are filled in only when
// JWT decode succeeds.
type JWTRecord struct {
	ReceivedAt string `json:"received_at"`
	Token      string `json:"token"`
	Sub        string `json:"sub,omitempty"`
	Iss        string `json:"iss,omitempty"`
	Exp        int64  `json:"exp,omitempty"`
	Alg        string `json:"alg,omitempty"`
}

// DoorbellRecord is the persisted shape of last_doorbell.json.
//
// MQTTRequestID is the top-level requestId from the /remote_view
// RPC envelope (UDM's MQTT-level tracking id). RequestID,
// DeviceID, RoomID, CancelToken, CreateTime, Capabilities,
// DeviceType, ClearRequestID are extracted from the inner
// protobuf submessage carried in the same RPC.
//
// Field-number mapping verified by saison-11-05 live capture
// (13:54 - 13:55, 11. May 2026):
//
//	field_1  -> RequestID        (UUID, session id UDM uses for the call)
//	field_5  -> DeviceID         (12-char MAC hex of the calling intercom)
//	field_7  -> ClearRequestID   (UUID for persistent session cleanup; NOT the match key)
//	field_9  -> RoomID           (WebRTC room string)
//	field_10 -> CancelToken      (32-char one-shot token; matches cancel.field_1)
//	field_13 -> Capabilities     (repeated string)
//	field_14 -> CreateTime       (unix seconds, varint)
//	field_15 -> DeviceType       (e.g. "UA-Intercom")
//
// Hypotheses (not live-confirmed):
//
//	field_4  -> ReasonCode       (varint, initial value 0, semantics unclear)
//	field_12 -> in_or_out        (suspected)
//	field_16 -> ReasonCode alt   (suspected)
//
// Cancel-side fields are nil until a /cancel_doorbell_notification
// arrives. The cancel body carries the cancel-token at field_1;
// see CancelDoorbell match logic.
type DoorbellRecord struct {
	ReceivedAt          string         `json:"received_at"`
	MQTTRequestID       string         `json:"mqtt_request_id,omitempty"`
	RequestID           string         `json:"request_id,omitempty"`
	ClearRequestID      string         `json:"clear_request_id,omitempty"`
	DeviceID            string         `json:"device_id,omitempty"`
	DeviceType          string         `json:"device_type,omitempty"`
	RoomID              string         `json:"room_id,omitempty"`
	CancelToken         string         `json:"cancel_token,omitempty"`
	CreateTime          int64          `json:"create_time,omitempty"`
	Capabilities        []string       `json:"capabilities,omitempty"`
	ReasonCode          int            `json:"reason_code,omitempty"`
	RawBody             string         `json:"raw_body,omitempty"`
	SubmessageFields    map[string]any `json:"submessage_fields,omitempty"`
	CancelledAt         *string        `json:"cancelled_at"`
	CancelMQTTRequestID *string        `json:"cancel_mqtt_request_id"`
	CancelReasonCode    *int           `json:"cancel_reason_code"`
	CancelRawBody       *string        `json:"cancel_raw_body"`
	CancelFields        map[string]any `json:"cancel_fields"`
}

// RuntimeConfig is the persisted shape of runtime_config.json.
// All fields are optional; merge logic preserves prior values.
type RuntimeConfig struct {
	ReceivedAt       string            `json:"received_at"`
	LocationID       string            `json:"location_id,omitempty"`
	LiveViewTimeout  int               `json:"live_view_timeout,omitempty"`
	TimezoneName     string            `json:"timezone_name,omitempty"`
	TimeFormat       int               `json:"time_format,omitempty"`
	Lockdown         bool              `json:"lockdown"`
	CertFingerprints map[string]string `json:"cert_fingerprints,omitempty"`
	IntercomMap      []map[string]any  `json:"intercom_map,omitempty"`
	Extras           map[string]any    `json:"extras,omitempty"`
}

// jwtPattern matches a JWT-shaped token (header.payload.signature,
// all base64url-no-padding). The header always starts with eyJ
// because the JSON object begins with {".
var jwtPattern = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)

// UpdateTokens persists the JWT carried in /update_tokens and
// merges the runtime config fields the same RPC delivers.
func (h *Handler) UpdateTokens(requestID string, body []byte) ([]byte, error) {
	if h == nil || h.Store == nil {
		return nil, errors.New("handlers: nil receiver or store")
	}
	now := time.Now().Format(time.RFC3339)
	fields, _ := proto.DecodeRPCRequestBody(body)

	rec := JWTRecord{ReceivedAt: now}
	jwt := findJWT(body)
	if jwt != "" {
		rec.Token = jwt
		if claims, alg, ok := decodeJWTClaims(jwt); ok {
			rec.Sub, _ = claims["sub"].(string)
			rec.Iss, _ = claims["iss"].(string)
			if exp, ok := claims["exp"].(float64); ok {
				rec.Exp = int64(exp)
			}
			rec.Alg = alg
		}
		data, _ := json.MarshalIndent(rec, "", "  ")
		if err := h.Store.WriteFile(h.MockID, "jwt.json", data, 0o600); err != nil {
			h.Log.Warnf("mqtt: handler /update_tokens persist jwt: %v", err)
		} else {
			h.Log.Infof("mqtt: handler /update_tokens persisted jwt exp=%d sub=%s",
				rec.Exp, rec.Sub)
		}
	} else {
		h.Log.Warnf("mqtt: handler /update_tokens no jwt found in body")
	}

	h.mergeRuntimeConfig(fields, now)
	return proto.EncodeRPCResponse("/update_tokens", requestID, "success"), nil
}

// UpdateConfigs merges runtime config fields delivered without a
// fresh JWT.
func (h *Handler) UpdateConfigs(requestID string, body []byte) ([]byte, error) {
	if h == nil || h.Store == nil {
		return nil, errors.New("handlers: nil receiver or store")
	}
	now := time.Now().Format(time.RFC3339)
	fields, _ := proto.DecodeRPCRequestBody(body)
	count := h.mergeRuntimeConfig(fields, now)
	h.Log.Infof("mqtt: handler /update_configs merged config fields=%d", count)
	return proto.EncodeRPCResponse("/update_configs", requestID, "success"), nil
}

// RemoteView persists one doorbell event to last_doorbell.json.
// Extracts session-level identifiers from the inner protobuf
// submessage via the field-number-to-name mapping documented on
// DoorbellRecord. Falls back to empty strings for any missing
// field; the raw body always lands in raw_body for offline
// analysis.
func (h *Handler) RemoteView(requestID string, body []byte) ([]byte, error) {
	if h == nil || h.Store == nil {
		return nil, errors.New("handlers: nil receiver or store")
	}
	fields, _ := proto.DecodeRPCRequestBody(body)
	sub, _ := fields["_submessage"].(map[string]any)

	rec := DoorbellRecord{
		ReceivedAt:       time.Now().Format(time.RFC3339),
		MQTTRequestID:    requestID,
		RequestID:        stringField(sub, "field_1"),
		ClearRequestID:   stringField(sub, "field_7"),
		DeviceID:         stringField(sub, "field_5"),
		DeviceType:       stringField(sub, "field_15"),
		RoomID:           stringField(sub, "field_9"),
		CancelToken:      stringField(sub, "field_10"),
		CreateTime:       int64Field(sub, "field_14"),
		Capabilities:     stringsField(sub, "field_13"),
		ReasonCode:       intField(sub, "field_4"),
		RawBody:          base64.StdEncoding.EncodeToString(body),
		SubmessageFields: sub,
	}
	data, _ := json.MarshalIndent(rec, "", "  ")
	if err := h.Store.WriteFile(h.MockID, "last_doorbell.json", data, 0o644); err != nil {
		h.Log.Warnf("mqtt: handler /remote_view persist: %v", err)
	}
	h.Log.Infof("mqtt: handler /remote_view DOORBELL device_id=%s request_id=%s room_id=%s",
		rec.DeviceID, rec.RequestID, rec.RoomID)
	if h.OnDoorbell != nil {
		h.OnDoorbell(rec, body)
	}
	return proto.EncodeRPCResponse("/remote_view", requestID, "success"), nil
}

// CancelDoorbell stamps the cancel time, mqtt-request-id, raw
// body and submessage fields onto last_doorbell.json. If no
// prior record exists, writes a minimal one so the cancel is
// never lost. Logs whether field_7 of the cancel submessage
// matches the persisted ClearRequestID for saison-11 hypothesis
// verification.
func (h *Handler) CancelDoorbell(requestID string, body []byte) ([]byte, error) {
	if h == nil || h.Store == nil {
		return nil, errors.New("handlers: nil receiver or store")
	}
	fields, _ := proto.DecodeRPCRequestBody(body)
	cancelSub, _ := fields["_submessage"].(map[string]any)

	reasonCode := 0
	if rs, ok := fields["reason_code"].(string); ok {
		reasonCode, _ = strconv.Atoi(rs)
	}
	if rc := intField(cancelSub, "field_4"); rc != 0 {
		reasonCode = rc
	}

	now := time.Now().Format(time.RFC3339)

	var rec DoorbellRecord
	path := filepath.Join(h.Store.MockDir(h.MockID), "last_doorbell.json")
	if existing, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(existing, &rec)
	}
	if rec.ReceivedAt == "" {
		rec.ReceivedAt = now
	}

	rec.CancelledAt = &now
	cancelMqttID := requestID
	rec.CancelMQTTRequestID = &cancelMqttID
	rcCopy := reasonCode
	rec.CancelReasonCode = &rcCopy
	cancelRawB64 := base64.StdEncoding.EncodeToString(body)
	rec.CancelRawBody = &cancelRawB64
	rec.CancelFields = cancelSub

	if rec.CancelToken != "" && cancelSub != nil {
		if got := stringField(cancelSub, "field_1"); got != "" {
			if got == rec.CancelToken {
				h.Log.Infof("mqtt: cancel matched cancel_token=%s", got)
			} else {
				h.Log.Warnf("mqtt: cancel field_1=%s does NOT match remote_view cancel_token=%s",
					got, rec.CancelToken)
			}
		}
	}

	data, _ := json.MarshalIndent(rec, "", "  ")
	if err := h.Store.WriteFile(h.MockID, "last_doorbell.json", data, 0o644); err != nil {
		h.Log.Warnf("mqtt: handler /cancel_doorbell_notification persist: %v", err)
	}
	h.Log.Infof("mqtt: handler /cancel_doorbell_notification cancelled request_id=%s reason=%d",
		requestID, reasonCode)
	if h.OnDoorbellCancel != nil {
		h.OnDoorbellCancel(rec, body)
	}
	return proto.EncodeRPCResponse("/cancel_doorbell_notification", requestID, "success"), nil
}

// stringField returns sub[name] as a string, or "" if absent or
// not a string.
func stringField(sub map[string]any, name string) string {
	if sub == nil {
		return ""
	}
	s, _ := sub[name].(string)
	return s
}

// intField returns sub[name] as an int (best-effort: accepts
// int64 and numeric strings), or 0 if absent.
func intField(sub map[string]any, name string) int {
	if sub == nil {
		return 0
	}
	switch v := sub[name].(type) {
	case int64:
		return int(v)
	case int:
		return v
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}

// int64Field returns sub[name] as int64 (varint values land as
// int64 from the protobuf submessage decoder).
func int64Field(sub map[string]any, name string) int64 {
	if sub == nil {
		return 0
	}
	switch v := sub[name].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

// stringsField returns sub[name] as []string. A repeated wire
// field from the submessage decoder arrives as []any holding
// strings; a single non-repeated occurrence arrives as string.
func stringsField(sub map[string]any, name string) []string {
	if sub == nil {
		return nil
	}
	switch v := sub[name].(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// mergeRuntimeConfig reads any prior runtime_config.json, overlays
// the new fields, and writes the merged result. Returns the count
// of fields applied from the new RPC (for logging).
func (h *Handler) mergeRuntimeConfig(fields map[string]any, now string) int {
	path := filepath.Join(h.Store.MockDir(h.MockID), "runtime_config.json")
	var cfg RuntimeConfig
	if existing, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(existing, &cfg)
	}
	if cfg.CertFingerprints == nil {
		cfg.CertFingerprints = map[string]string{}
	}
	if cfg.Extras == nil {
		cfg.Extras = map[string]any{}
	}
	cfg.ReceivedAt = now

	applied := 0
	apply := func(name string, set func(string)) {
		if v, ok := fields[name].(string); ok {
			set(v)
			applied++
		}
	}
	apply("timezone_name", func(v string) { cfg.TimezoneName = v })
	apply("live_view_timeout", func(v string) {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.LiveViewTimeout = n
		}
	})
	apply("time_format", func(v string) {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.TimeFormat = n
		}
	})
	apply("lockdown", func(v string) {
		cfg.Lockdown = strings.EqualFold(v, "true")
	})
	apply("http_cert_fingerprint", func(v string) { cfg.CertFingerprints["http"] = v })
	apply("mqtt_cert_fingerprint", func(v string) { cfg.CertFingerprints["mqtt"] = v })
	apply("ucore_http_cert_fingerprint", func(v string) { cfg.CertFingerprints["ucore_http"] = v })

	// intercoms is a JSON-string-payloaded entry holding the
	// intercom list; parse it and lift location_id out of the
	// first entry to the top-level field.
	if raw, ok := fields["intercoms"].(string); ok && raw != "" {
		var entries []map[string]any
		if err := json.Unmarshal([]byte(raw), &entries); err == nil {
			cfg.IntercomMap = entries
			applied++
			if len(entries) > 0 {
				if loc, ok := entries[0]["location_id"].(string); ok && loc != "" {
					cfg.LocationID = loc
				}
			}
		} else {
			h.Log.Warnf("mqtt: handler intercoms json decode failed: %v", err)
		}
	}

	// Anything else we observed but did not model explicitly
	// lands under extras for future analysis.
	known := map[string]bool{
		"path": true, "requestId": true,
		"timezone_name": true, "live_view_timeout": true,
		"time_format": true, "lockdown": true,
		"http_cert_fingerprint": true, "mqtt_cert_fingerprint": true,
		"ucore_http_cert_fingerprint": true, "intercoms": true,
	}
	for k, v := range fields {
		if known[k] {
			continue
		}
		cfg.Extras[k] = v
	}

	data, _ := json.MarshalIndent(cfg, "", "  ")
	if err := h.Store.WriteFile(h.MockID, "runtime_config.json", data, 0o644); err != nil {
		h.Log.Warnf("mqtt: handler persist runtime_config: %v", err)
	}
	return applied
}

// findJWT scans body bytes for a JWT-shaped token. Returns the
// first match or empty string.
func findJWT(body []byte) string {
	if m := jwtPattern.Find(body); m != nil {
		return string(m)
	}
	return ""
}

// decodeJWTClaims base64-decodes the payload segment (between the
// two dots) and unmarshals it as JSON. Returns the claims map and
// the alg from the header. Reports (nil, "", false) on any error.
func decodeJWTClaims(token string) (map[string]any, string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, "", false
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, "", false
	}
	var header map[string]any
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, "", false
	}
	alg, _ := header["alg"].(string)

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, "", false
	}
	var claims map[string]any
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil, "", false
	}
	return claims, alg, true
}
