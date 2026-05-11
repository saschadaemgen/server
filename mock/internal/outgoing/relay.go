// Package outgoing builds and publishes the Mock's own RPCs to UDM
// via the active MQTT connection. Saison 11-07 scope: only the
// /relay/unlock method in "intercom-call" mode (caller specifies
// hub, intercom and bell). Method-specific dispatch follows the
// saison-11 reverse-engineered wire format documented in
// docs/wire-format.md.
package outgoing

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
)

// MQTTPublisher is the minimal surface the outgoing package needs
// to push a payload onto a topic. Implementations: the real paho-
// backed mqtt.Client and any fake used in tests.
type MQTTPublisher interface {
	Publish(topic string, payload []byte) error
}

// Logger is the minimal logging interface, matching the shape used
// in the mqtt and handlers packages so the same simpleLogger
// satisfies all three.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

// requestIDAlphabet is the character pool for the 5-byte random
// request id. Matches what UDM/Viewer use empirically: mixed-case
// ASCII letters plus digits.
const requestIDAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// RelayPublisher holds the dependencies needed to send relay-style
// RPCs from this Mock. Constructed once at startup; safe to reuse
// across HTTP requests.
type RelayPublisher struct {
	mqtt      MQTTPublisher
	senderMAC string // 12-char hex, no colons; used in topic from-segment AND body field_10
	udmID     string // UDM MAC without colons; used in topic <udm-id> segment
	log       Logger
}

// NewRelayPublisher wires the publisher. senderMAC is the audit
// identity that lands in the body's field_10 and in the topic from-
// segment; pass the Mock's own identity.ID. udmID is the UDM MAC
// (no colons) from the adoption bundle.
func NewRelayPublisher(mqtt MQTTPublisher, senderMAC, udmID string, log Logger) *RelayPublisher {
	return &RelayPublisher{mqtt: mqtt, senderMAC: senderMAC, udmID: udmID, log: log}
}

// Unlock publishes a /relay/unlock RPC in the intercom-call mode to
// the given hub. Returns the generated request id.
func (r *RelayPublisher) Unlock(hubMAC, intercomMAC, bellID string) (string, error) {
	if r == nil || r.mqtt == nil {
		return "", errors.New("outgoing: nil publisher")
	}
	return PublishRelayUnlock(r.mqtt, r.udmID, hubMAC, intercomMAC, r.senderMAC, bellID, r.log)
}

// PublishRelayUnlock builds a saison-11-05-shape /relay/unlock RPC
// body (intercom-call mode) and publishes it via mqtt. Returns the
// generated 5-char request id.
//
// Wire format: see docs/wire-format.md and the LHoFs goldmine in
// mock/internal/outgoing/testdata/. Body has no outer 0x12 wrapper
// (live capture confirmed device-to-UDM requests follow the same
// wrapper-free shape as UDM-to-device requests).
func PublishRelayUnlock(
	mqtt MQTTPublisher,
	udmID string,
	hubMAC string,
	intercomMAC string,
	senderMAC string,
	bellID string,
	log Logger,
) (string, error) {
	if mqtt == nil {
		return "", errors.New("outgoing: mqtt publisher is nil")
	}
	if udmID == "" || hubMAC == "" || senderMAC == "" {
		return "", errors.New("outgoing: udmID, hubMAC and senderMAC are required")
	}
	requestID, err := newRequestID()
	if err != nil {
		return "", fmt.Errorf("outgoing: gen request id: %w", err)
	}
	body := buildRelayUnlockBody(requestID, intercomMAC, senderMAC, bellID)
	topic := fmt.Sprintf("/uctrl/%s/device/%s/rpc/%s/request", udmID, hubMAC, senderMAC)
	if log != nil {
		log.Infof("mqtt: publishing /relay/unlock to %s req_id=%s", topic, requestID)
	}
	if err := mqtt.Publish(topic, body); err != nil {
		if log != nil {
			log.Warnf("mqtt: /relay/unlock publish failed: %v", err)
		}
		return requestID, err
	}
	if log != nil {
		log.Infof("mqtt: relay_unlock published, fire-and-forget (response tracking deferred to saison 12)")
	}
	return requestID, nil
}

// buildRelayUnlockBody emits the saison-11-05-verified intercom-
// call layout. The output matches the goldmine LHoFs byte-for-byte
// when given the same inputs (intercomMAC, senderMAC, bellID).
//
// Body layout (no outer wrapper):
//
//	0a + len + (key=requestId, value=<requestID>)
//	0a + len + (key=path,      value=/relay/unlock)
//	12 + len + submessage
//
// Submessage layout (field order matches goldmine):
//
//	field_1  string "intercom"
//	field_4  string ""
//	field_5  string bellID
//	field_6  varint 0
//	field_7  string intercomMAC (12 hex chars)
//	field_8  string "both"
//	field_9  string ""
//	field_10 string senderMAC   (12 hex chars)
func buildRelayUnlockBody(requestID, intercomMAC, senderMAC, bellID string) []byte {
	var sub []byte
	sub = appendStringField(sub, 1, "intercom")
	sub = appendStringField(sub, 4, "")
	sub = appendStringField(sub, 5, bellID)
	sub = appendVarintField(sub, 6, 0)
	sub = appendStringField(sub, 7, intercomMAC)
	sub = appendStringField(sub, 8, "both")
	sub = appendStringField(sub, 9, "")
	sub = appendStringField(sub, 10, senderMAC)

	var body []byte
	body = appendMapEntry(body, "requestId", requestID)
	body = appendMapEntry(body, "path", "/relay/unlock")
	body = append(body, 0x12) // (field 2 << 3) | wire-type 2
	body = appendVarint(body, uint64(len(sub)))
	body = append(body, sub...)
	return body
}

// appendMapEntry writes one strict map-entry block:
// 0x0a + len + (0x0a + len + key | 0x12 + len + value).
func appendMapEntry(dst []byte, key, value string) []byte {
	var inner []byte
	inner = append(inner, 0x0a)
	inner = appendVarint(inner, uint64(len(key)))
	inner = append(inner, key...)
	inner = append(inner, 0x12)
	inner = appendVarint(inner, uint64(len(value)))
	inner = append(inner, value...)

	dst = append(dst, 0x0a)
	dst = appendVarint(dst, uint64(len(inner)))
	return append(dst, inner...)
}

// appendStringField writes (fieldNum << 3 | 2) + len + bytes.
func appendStringField(dst []byte, fieldNum uint64, value string) []byte {
	dst = appendVarint(dst, (fieldNum<<3)|2)
	dst = appendVarint(dst, uint64(len(value)))
	return append(dst, value...)
}

// appendVarintField writes (fieldNum << 3 | 0) + varint(value).
func appendVarintField(dst []byte, fieldNum, value uint64) []byte {
	dst = appendVarint(dst, (fieldNum<<3)|0)
	return appendVarint(dst, value)
}

// appendVarint writes the protobuf varint encoding of v.
func appendVarint(dst []byte, v uint64) []byte {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], v)
	return append(dst, buf[:n]...)
}

// newRequestID returns a 5-character random ASCII string suitable
// for the requestId top-level map-entry, matching the empirical
// 5-char pattern UDM and the real Viewer use (e.g. "LHoFs").
func newRequestID() (string, error) {
	var raw [5]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	out := make([]byte, 5)
	for i, b := range raw {
		out[i] = requestIDAlphabet[int(b)%len(requestIDAlphabet)]
	}
	return string(out), nil
}
