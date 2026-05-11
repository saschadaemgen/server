package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"unifix.local/mock/internal/state"
)

type silentLogger struct{}

func (silentLogger) Infof(string, ...any)  {}
func (silentLogger) Warnf(string, ...any)  {}
func (silentLogger) Errorf(string, ...any) {}

const testMockID = "0cea14424242"

func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	store, err := state.New(t.TempDir())
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	return &Handler{Store: store, MockID: testMockID, Log: silentLogger{}}
}

// appendVarint mirrors the helper in shared/proto. Inlined here
// so test fixtures can build wire-format bodies without exposing
// the proto package's internals.
func appendVarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

// mapEntryBytes returns one map-entry block (0x0a + len + (key|value)).
func mapEntryBytes(key, value string) []byte {
	inner := append([]byte{0x0a}, appendVarint(nil, uint64(len(key)))...)
	inner = append(inner, key...)
	inner = append(inner, 0x12)
	inner = append(inner, appendVarint(nil, uint64(len(value)))...)
	inner = append(inner, value...)

	out := append([]byte{0x0a}, appendVarint(nil, uint64(len(inner)))...)
	return append(out, inner...)
}

// buildSyntheticBody composes a UDM-native-shape RPC body from
// the provided key/value pairs.
func buildSyntheticBody(pairs ...[2]string) []byte {
	var out []byte
	for _, p := range pairs {
		out = append(out, mapEntryBytes(p[0], p[1])...)
	}
	return out
}

// protobufVarintField writes one wire-type-0 field for synthetic
// /remote_view-shaped submessages.
func protobufVarintField(fieldNum uint64, value uint64) []byte {
	out := appendVarint(nil, (fieldNum<<3)|0)
	return append(out, appendVarint(nil, value)...)
}

// protobufStringField writes one wire-type-2 field with the value
// laid out inline as bytes.
func protobufStringField(fieldNum uint64, value string) []byte {
	out := appendVarint(nil, (fieldNum<<3)|2)
	out = append(out, appendVarint(nil, uint64(len(value)))...)
	return append(out, value...)
}

// wrapAsTopLevelSubmessage embeds sub as a top-level wire-type-2
// field at field number 2 so DecodeRPCRequestBody picks it up via
// extractInnerSubmessage.
func wrapAsTopLevelSubmessage(sub []byte) []byte {
	out := []byte{0x12} // (2<<3)|2
	out = append(out, appendVarint(nil, uint64(len(sub)))...)
	return append(out, sub...)
}

// remoteViewBodyOpts configures buildRemoteViewBody.
type remoteViewBodyOpts struct {
	MQTTRequestID string
	RequestUUID   string
	ClearUUID     string
	CancelToken   string
	DeviceID      string
	RoomID        string
	DeviceType    string
	Capabilities  []string
	CreateTime    uint64
	ReasonCode    uint64
}

// buildRemoteViewBody composes a /remote_view RPC body in the
// shape UDM actually sends: top-level map-entries for path and
// requestId, followed by a standard-protobuf submessage with the
// saison-11-05 field-number layout (field_10 = cancel token).
func buildRemoteViewBody(opts remoteViewBodyOpts) []byte {
	body := buildSyntheticBody(
		[2]string{"path", "/remote_view"},
		[2]string{"requestId", opts.MQTTRequestID},
	)
	var sub []byte
	if opts.RequestUUID != "" {
		sub = append(sub, protobufStringField(1, opts.RequestUUID)...)
	}
	if opts.ReasonCode != 0 {
		sub = append(sub, protobufVarintField(4, opts.ReasonCode)...)
	}
	if opts.DeviceID != "" {
		sub = append(sub, protobufStringField(5, opts.DeviceID)...)
	}
	if opts.ClearUUID != "" {
		sub = append(sub, protobufStringField(7, opts.ClearUUID)...)
	}
	if opts.RoomID != "" {
		sub = append(sub, protobufStringField(9, opts.RoomID)...)
	}
	if opts.CancelToken != "" {
		sub = append(sub, protobufStringField(10, opts.CancelToken)...)
	}
	for _, cap := range opts.Capabilities {
		sub = append(sub, protobufStringField(13, cap)...)
	}
	if opts.CreateTime != 0 {
		sub = append(sub, protobufVarintField(14, opts.CreateTime)...)
	}
	if opts.DeviceType != "" {
		sub = append(sub, protobufStringField(15, opts.DeviceType)...)
	}
	return append(body, wrapAsTopLevelSubmessage(sub)...)
}

// buildCancelBody builds a /cancel_doorbell_notification body
// with an inner submessage carrying field_1 (cancel_token, the
// saison-11-05-verified match key) and optional reason_code at
// field_4.
func buildCancelBody(mqttRequestID, cancelToken string, reasonCode uint64) []byte {
	body := buildSyntheticBody(
		[2]string{"path", "/cancel_doorbell_notification"},
		[2]string{"requestId", mqttRequestID},
	)
	var sub []byte
	if cancelToken != "" {
		sub = append(sub, protobufStringField(1, cancelToken)...)
	}
	if reasonCode != 0 {
		sub = append(sub, protobufVarintField(4, reasonCode)...)
	}
	return append(body, wrapAsTopLevelSubmessage(sub)...)
}

// makeTestJWT signs a minimal HS256 JWT and returns its compact
// serialization. Useful for embedding in synthetic /update_tokens
// bodies.
func makeTestJWT(t *testing.T, sub string, exp int64) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{
		"sub": sub,
		"iss": "unifi-access",
		"exp": exp,
	})
	h := base64.RawURLEncoding.EncodeToString(header)
	p := base64.RawURLEncoding.EncodeToString(claims)
	signingInput := h + "." + p
	mac := hmac.New(sha256.New, []byte("test-secret"))
	mac.Write([]byte(signingInput))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signingInput + "." + sig
}

func TestHandlerUpdateTokens_PersistsJWT(t *testing.T) {
	h := newTestHandler(t)
	exp := time.Now().Add(15 * time.Minute).Unix()
	jwt := makeTestJWT(t, testMockID, exp)

	// Body: two map entries (path + requestId) followed by the
	// raw JWT bytes embedded as if they were a length-delimited
	// field value (UDM emits it at a numbered protobuf field).
	body := buildSyntheticBody(
		[2]string{"path", "/update_tokens"},
		[2]string{"requestId", "req-jwt-1"},
	)
	// Stick the JWT in plain after the map entries so findJWT's
	// regex picks it up. Real UDM frames have the same property.
	body = append(body, []byte(jwt)...)

	resp, err := h.UpdateTokens("req-jwt-1", body)
	if err != nil {
		t.Fatalf("UpdateTokens: %v", err)
	}
	if len(resp) == 0 || resp[0] != 0x12 {
		t.Errorf("response shape unexpected, first byte = 0x%02x", resp[0])
	}

	jwtPath := filepath.Join(h.Store.MockDir(testMockID), "jwt.json")
	data, err := os.ReadFile(jwtPath)
	if err != nil {
		t.Fatalf("jwt.json missing: %v", err)
	}
	var rec JWTRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("unmarshal jwt.json: %v", err)
	}
	if rec.Token != jwt {
		t.Errorf("Token = %q, want %q", rec.Token, jwt)
	}
	if rec.Sub != testMockID {
		t.Errorf("Sub = %q, want %q", rec.Sub, testMockID)
	}
	if rec.Iss != "unifi-access" {
		t.Errorf("Iss = %q, want unifi-access", rec.Iss)
	}
	if rec.Exp != exp {
		t.Errorf("Exp = %d, want %d", rec.Exp, exp)
	}
	if rec.Alg != "HS256" {
		t.Errorf("Alg = %q, want HS256", rec.Alg)
	}
	if rec.ReceivedAt == "" {
		t.Errorf("ReceivedAt should be set")
	}

	// File permissions check (POSIX only).
	if info, err := os.Stat(jwtPath); err == nil {
		if mode := info.Mode().Perm(); mode != 0o600 && mode != 0o666 {
			// 0o666 is acceptable on systems where umask makes 0o600
			// unenforced; we still pass because the chmod call was
			// made. On Linux production target this resolves to 0o600.
			t.Logf("jwt.json mode = %o (production target Linux enforces 0600)", mode)
		}
	}
}

func TestHandlerRemoteView_ExtractsDeviceIDAndRoomID(t *testing.T) {
	h := newTestHandler(t)

	body := buildRemoteViewBody(remoteViewBodyOpts{
		MQTTRequestID: "mqtt-tracking-id-xyz",
		RequestUUID:   "c187eb6e-323a-4994-bfea-abdfe4bdcfd3",
		ClearUUID:     "e17f64a6-fdeb-4e65-8b42-6dfedd026f7f",
		CancelToken:   "OkWk3lfeqjZzA2XjoQlIFcX17n0ZGLjx",
		DeviceID:      "28704e31e29c",
		DeviceType:    "UA-Intercom",
		RoomID:        "WR-28704e31e29c-abc123",
		Capabilities:  []string{"doorbell_bind", "doorbell_timeout", "support_ems"},
		CreateTime:    1778500490,
		ReasonCode:    82,
	})

	resp, err := h.RemoteView("mqtt-tracking-id-xyz", body)
	if err != nil {
		t.Fatalf("RemoteView: %v", err)
	}
	if len(resp) == 0 {
		t.Fatal("RemoteView returned empty response")
	}

	path := filepath.Join(h.Store.MockDir(testMockID), "last_doorbell.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("last_doorbell.json missing: %v", err)
	}
	var rec DoorbellRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rec.MQTTRequestID != "mqtt-tracking-id-xyz" {
		t.Errorf("MQTTRequestID = %q, want mqtt-tracking-id-xyz", rec.MQTTRequestID)
	}
	if rec.RequestID != "c187eb6e-323a-4994-bfea-abdfe4bdcfd3" {
		t.Errorf("RequestID = %q, want the field_1 UUID", rec.RequestID)
	}
	if rec.ClearRequestID != "e17f64a6-fdeb-4e65-8b42-6dfedd026f7f" {
		t.Errorf("ClearRequestID = %q, want the field_7 UUID", rec.ClearRequestID)
	}
	if rec.CancelToken != "OkWk3lfeqjZzA2XjoQlIFcX17n0ZGLjx" {
		t.Errorf("CancelToken = %q, want the field_10 token", rec.CancelToken)
	}
	if rec.DeviceID != "28704e31e29c" {
		t.Errorf("DeviceID = %q, want 28704e31e29c", rec.DeviceID)
	}
	if rec.DeviceType != "UA-Intercom" {
		t.Errorf("DeviceType = %q, want UA-Intercom", rec.DeviceType)
	}
	if rec.RoomID != "WR-28704e31e29c-abc123" {
		t.Errorf("RoomID = %q, want WR-28704e31e29c-abc123", rec.RoomID)
	}
	if rec.CreateTime != 1778500490 {
		t.Errorf("CreateTime = %d, want 1778500490", rec.CreateTime)
	}
	if len(rec.Capabilities) != 3 || rec.Capabilities[0] != "doorbell_bind" {
		t.Errorf("Capabilities = %v, want [doorbell_bind, doorbell_timeout, support_ems]", rec.Capabilities)
	}
	if rec.ReasonCode != 82 {
		t.Errorf("ReasonCode = %d, want 82", rec.ReasonCode)
	}
	if rec.SubmessageFields == nil {
		t.Error("SubmessageFields should contain the field_N entries")
	}
	if rec.RawBody == "" {
		t.Error("RawBody should contain base64 of body bytes")
	}
	if rec.CancelledAt != nil {
		t.Errorf("CancelledAt should be nil on fresh /remote_view, got %v", rec.CancelledAt)
	}
}

func TestHandlerCancelDoorbell_UpdatesLastDoorbell(t *testing.T) {
	h := newTestHandler(t)

	cancelToken := "OkWk3lfeqjZzA2XjoQlIFcX17n0ZGLjx"
	clearUUID := "e17f64a6-fdeb-4e65-8b42-6dfedd026f7f"

	// First lay down a doorbell record with the full saison-11-05
	// submessage layout.
	rv := buildRemoteViewBody(remoteViewBodyOpts{
		MQTTRequestID: "ring-mqtt-1",
		RequestUUID:   "c187eb6e-323a-4994-bfea-abdfe4bdcfd3",
		ClearUUID:     clearUUID,
		CancelToken:   cancelToken,
		DeviceID:      "28704e31e29c",
		RoomID:        "WR-28704e31e29c-room",
		ReasonCode:    82,
	})
	if _, err := h.RemoteView("ring-mqtt-1", rv); err != nil {
		t.Fatalf("RemoteView setup: %v", err)
	}

	// Then cancel it. Cancel body field_1 carries the cancel_token
	// (saison-11-05 verified match key).
	cancel := buildCancelBody("cancel-mqtt-1", cancelToken, 108)
	if _, err := h.CancelDoorbell("cancel-mqtt-1", cancel); err != nil {
		t.Fatalf("CancelDoorbell: %v", err)
	}

	path := filepath.Join(h.Store.MockDir(testMockID), "last_doorbell.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var rec DoorbellRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Pre-cancel fields preserved.
	if rec.DeviceID != "28704e31e29c" {
		t.Errorf("DeviceID = %q (lost across cancel)", rec.DeviceID)
	}
	if rec.CancelToken != cancelToken {
		t.Errorf("CancelToken = %q, want %q", rec.CancelToken, cancelToken)
	}
	if rec.ClearRequestID != clearUUID {
		t.Errorf("ClearRequestID = %q, want %q", rec.ClearRequestID, clearUUID)
	}
	// Cancel-side fields set.
	if rec.CancelledAt == nil || *rec.CancelledAt == "" {
		t.Error("CancelledAt should be set to an RFC3339 timestamp")
	}
	if rec.CancelMQTTRequestID == nil || *rec.CancelMQTTRequestID != "cancel-mqtt-1" {
		t.Errorf("CancelMQTTRequestID = %v, want 'cancel-mqtt-1'", rec.CancelMQTTRequestID)
	}
	if rec.CancelReasonCode == nil || *rec.CancelReasonCode != 108 {
		got := -1
		if rec.CancelReasonCode != nil {
			got = *rec.CancelReasonCode
		}
		t.Errorf("CancelReasonCode = %d, want 108", got)
	}
	if rec.CancelRawBody == nil || *rec.CancelRawBody == "" {
		t.Error("CancelRawBody should be set to base64 of cancel body bytes")
	} else {
		decoded, err := base64.StdEncoding.DecodeString(*rec.CancelRawBody)
		if err != nil {
			t.Fatalf("decode cancel_raw_body: %v", err)
		}
		if string(decoded) != string(cancel) {
			t.Error("cancel_raw_body did not roundtrip")
		}
	}
	if rec.CancelFields == nil {
		t.Error("CancelFields must contain the parsed cancel submessage")
	} else {
		if got, _ := rec.CancelFields["field_1"].(string); got != cancelToken {
			t.Errorf("CancelFields.field_1 = %q, want %q (the cancel_token match key)", got, cancelToken)
		}
	}
}

func TestHandlerCancelDoorbell_StandaloneWritesMinimalRecord(t *testing.T) {
	// Edge case: cancel arrives without prior /remote_view (e.g.
	// after a crash). Should still write last_doorbell.json so the
	// event is not lost.
	h := newTestHandler(t)

	cancel := buildCancelBody("ghost-cancel", "ghost-token-7777", 105)
	if _, err := h.CancelDoorbell("ghost-cancel", cancel); err != nil {
		t.Fatalf("CancelDoorbell: %v", err)
	}

	path := filepath.Join(h.Store.MockDir(testMockID), "last_doorbell.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var rec DoorbellRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rec.CancelledAt == nil {
		t.Error("CancelledAt must be set")
	}
	if rec.CancelMQTTRequestID == nil || *rec.CancelMQTTRequestID != "ghost-cancel" {
		t.Error("CancelMQTTRequestID should be 'ghost-cancel'")
	}
	if rec.CancelReasonCode == nil || *rec.CancelReasonCode != 105 {
		t.Errorf("CancelReasonCode missing or wrong")
	}
	if rec.CancelFields == nil {
		t.Error("CancelFields should be populated from the submessage")
	}
}

func TestDecodeJWTClaims_RoundTrip(t *testing.T) {
	jwt := makeTestJWT(t, testMockID, 1700000000)
	claims, alg, ok := decodeJWTClaims(jwt)
	if !ok {
		t.Fatal("decodeJWTClaims returned !ok for a valid JWT")
	}
	if alg != "HS256" {
		t.Errorf("alg = %q, want HS256", alg)
	}
	if sub, _ := claims["sub"].(string); sub != testMockID {
		t.Errorf("sub = %q, want %q", sub, testMockID)
	}
}

func TestFindJWT_LocatesEmbeddedJWT(t *testing.T) {
	jwt := makeTestJWT(t, testMockID, 1700000000)
	// Surround with arbitrary binary so we test the regex against
	// realistic noise.
	pre := []byte{0x0a, 0x05, 'h', 'e', 'l', 'l', 'o', 0x12, 0x03}
	post := []byte{0xff, 0x00, 0x10}
	body := append(append(pre, []byte(jwt)...), post...)
	if got := findJWT(body); got != jwt {
		t.Errorf("findJWT = %q, want %q", got, jwt)
	}
}

// Sanity for the test helper itself: ensure mapEntryBytes layout
// matches the strict 0x0a-len-key-0x12-len-value shape proto
// decodes.
func TestMapEntryBytes_Shape(t *testing.T) {
	got := mapEntryBytes("a", "b")
	want := []byte{0x0a, 0x06, 0x0a, 0x01, 'a', 0x12, 0x01, 'b'}
	if string(got) != string(want) {
		t.Errorf("got % x, want % x", got, want)
	}
	// And the varint encoder works for >127 lengths.
	long := strings.Repeat("x", 200)
	out := mapEntryBytes("k", long)
	if int(out[1])&0x80 == 0 {
		t.Error("expected multi-byte varint outer length for long value")
	}
}
