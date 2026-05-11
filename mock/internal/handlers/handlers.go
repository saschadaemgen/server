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
type Handler struct {
	Store  *state.Store
	MockID string
	Log    Logger
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
// CancelledAt and CancelReasonCode are set by the cancel handler
// only.
type DoorbellRecord struct {
	ReceivedAt       string  `json:"received_at"`
	RequestID        string  `json:"request_id"`
	DeviceID         string  `json:"device_id,omitempty"`
	HubID            string  `json:"hub_id,omitempty"`
	RoomID           string  `json:"room_id,omitempty"`
	RawBody          string  `json:"raw_body,omitempty"`
	CancelledAt      *string `json:"cancelled_at"`
	CancelReasonCode *int    `json:"cancel_reason_code"`
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
func (h *Handler) RemoteView(requestID string, body []byte) ([]byte, error) {
	if h == nil || h.Store == nil {
		return nil, errors.New("handlers: nil receiver or store")
	}
	fields, _ := proto.DecodeRPCRequestBody(body)
	deviceID, _ := fields["device_id"].(string)
	hubID, _ := fields["hub_id"].(string)
	roomID, _ := fields["room_id"].(string)

	rec := DoorbellRecord{
		ReceivedAt: time.Now().Format(time.RFC3339),
		RequestID:  requestID,
		DeviceID:   deviceID,
		HubID:      hubID,
		RoomID:     roomID,
		RawBody:    base64.StdEncoding.EncodeToString(body),
	}
	data, _ := json.MarshalIndent(rec, "", "  ")
	if err := h.Store.WriteFile(h.MockID, "last_doorbell.json", data, 0o644); err != nil {
		h.Log.Warnf("mqtt: handler /remote_view persist: %v", err)
	}
	h.Log.Infof("mqtt: handler /remote_view DOORBELL device_id=%s hub_id=%s request_id=%s",
		deviceID, hubID, requestID)
	return proto.EncodeRPCResponse("/remote_view", requestID, "success"), nil
}

// CancelDoorbell stamps the cancel time and optional reason code
// onto last_doorbell.json. If no prior record exists, writes a
// minimal one so the cancel is never lost.
func (h *Handler) CancelDoorbell(requestID string, body []byte) ([]byte, error) {
	if h == nil || h.Store == nil {
		return nil, errors.New("handlers: nil receiver or store")
	}
	fields, _ := proto.DecodeRPCRequestBody(body)
	reasonCode := 0
	if rs, ok := fields["reason_code"].(string); ok {
		reasonCode, _ = strconv.Atoi(rs)
	}
	now := time.Now().Format(time.RFC3339)

	var rec DoorbellRecord
	path := filepath.Join(h.Store.MockDir(h.MockID), "last_doorbell.json")
	if existing, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(existing, &rec)
	}
	if rec.RequestID == "" {
		rec.RequestID = requestID
		rec.ReceivedAt = now
	} else if rec.RequestID != requestID {
		h.Log.Warnf("mqtt: handler /cancel_doorbell_notification request_id mismatch (got %s, last was %s)",
			requestID, rec.RequestID)
	}
	rec.CancelledAt = &now
	rcCopy := reasonCode
	rec.CancelReasonCode = &rcCopy

	data, _ := json.MarshalIndent(rec, "", "  ")
	if err := h.Store.WriteFile(h.MockID, "last_doorbell.json", data, 0o644); err != nil {
		h.Log.Warnf("mqtt: handler /cancel_doorbell_notification persist: %v", err)
	}
	h.Log.Infof("mqtt: handler /cancel_doorbell_notification cancelled request_id=%s reason=%d",
		requestID, reasonCode)
	return proto.EncodeRPCResponse("/cancel_doorbell_notification", requestID, "success"), nil
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

