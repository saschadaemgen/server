package outgoing

// /call_admin_result wire-format encoder.
//
// Saison 13-04.5-A reverse-engineered the body off a hardware UA
// Intercom Viewer (4 captures, 14 May 2026); see
// docs/carvilon-server-wire-format.md "/call_admin_result body schema" for the
// full hex sample plus side effects. Saison 13-04.5-B uses this
// encoder so the carvilon mieter "Ignorieren" / "Anruf beenden"
// buttons can silence the intercom immediately instead of waiting
// for the 30-second UDM-side timeout.
//
// All three doorbell-end scenarios (decline / answer-then-hangup /
// direct-hangup, plus the tuer-auf-implies-pickup quirk) emit the
// SAME body with status="denied"; outcome is implicit in UDM's
// internal state machine, not in the payload.

import (
	"errors"
	"fmt"
)

// CallAdminResultRequest holds the inputs for one /call_admin_result
// RPC. Both fields are required and length-checked.
type CallAdminResultRequest struct {
	// RequestID is the 5-char ASCII id the response echoes back.
	// Use newRequestID() for fresh ones.
	RequestID string
	// IntercomMAC is the 12-char lowercase hex MAC of the
	// intercom whose doorbell call is being rejected. Goes into
	// the audit field AND the topic.
	IntercomMAC string
}

// Encode produces the full /call_admin_result body (top-level map
// entries path + requestId followed by the inner submessage). The
// resulting byte slice is exactly 89 bytes for the saison-13
// reference inputs.
func (r *CallAdminResultRequest) Encode() ([]byte, error) {
	if len(r.RequestID) != 5 {
		return nil, fmt.Errorf("outgoing: call_admin_result requestId must be 5 chars, got %d", len(r.RequestID))
	}
	if len(r.IntercomMAC) != 12 {
		return nil, fmt.Errorf("outgoing: call_admin_result intercom mac must be 12 hex chars, got %d", len(r.IntercomMAC))
	}
	inner := buildCallAdminResultSubmessage(r.IntercomMAC)

	var body []byte
	body = appendMapEntry(body, "path", "/call_admin_result")
	body = appendMapEntry(body, "requestId", r.RequestID)
	// Inner submessage carried under field 2 (wire-type 2).
	body = append(body, 0x12)
	body = appendVarint(body, uint64(len(inner)))
	body = append(body, inner...)
	return body, nil
}

// buildCallAdminResultSubmessage emits the 39-byte inner body. Field
// numbering matches the live capture: field_1 "service", field_2
// "Access" (the namespace), field_3 "denied" (the verdict), field_4
// the audit intercom-mac.
func buildCallAdminResultSubmessage(intercomMAC string) []byte {
	var sub []byte
	sub = appendStringField(sub, 1, "service")
	sub = appendStringField(sub, 2, "Access")
	sub = appendStringField(sub, 3, "denied")
	sub = appendStringField(sub, 4, intercomMAC)
	return sub
}

// CallAdminResultTopic returns the MQTT topic the encoded body
// must be published to. Both the device-segment and the from-
// segment are the udm-id; the target IS the intercom but the
// topic shape places the udm-id in both ends because the request
// originates server-side (UA Companion / hardware viewer) on
// behalf of the user.
func CallAdminResultTopic(udmID, intercomMAC string) string {
	if udmID == "" || intercomMAC == "" {
		return ""
	}
	return fmt.Sprintf("/uctrl/%s/device/%s/rpc/%s/request", udmID, intercomMAC, udmID)
}

// CallAdminResultPublisher wires the encoder to a live MQTT
// publisher and remembers the udm-id once. Long-lived; safe to
// reuse across calls.
type CallAdminResultPublisher struct {
	mqtt  MQTTPublisher
	udmID string
	log   Logger
}

// NewCallAdminResultPublisher constructs the publisher.
func NewCallAdminResultPublisher(mqtt MQTTPublisher, udmID string, log Logger) *CallAdminResultPublisher {
	return &CallAdminResultPublisher{mqtt: mqtt, udmID: udmID, log: log}
}

// PublishReject builds and publishes a fresh /call_admin_result for
// the given intercom. Returns the generated request id so the
// caller can correlate logs.
func (p *CallAdminResultPublisher) PublishReject(intercomMAC string) (string, error) {
	if p == nil || p.mqtt == nil {
		return "", errors.New("outgoing: nil call_admin_result publisher")
	}
	if p.udmID == "" {
		return "", errors.New("outgoing: call_admin_result publisher has no udm id")
	}
	requestID, err := newRequestID()
	if err != nil {
		return "", fmt.Errorf("outgoing: gen request id: %w", err)
	}
	req := CallAdminResultRequest{RequestID: requestID, IntercomMAC: intercomMAC}
	body, err := req.Encode()
	if err != nil {
		return requestID, err
	}
	topic := CallAdminResultTopic(p.udmID, intercomMAC)
	if p.log != nil {
		p.log.Infof("mqtt: publishing /call_admin_result to %s req_id=%s intercom=%s",
			topic, requestID, intercomMAC)
	}
	if err := p.mqtt.Publish(topic, body); err != nil {
		if p.log != nil {
			p.log.Warnf("mqtt: /call_admin_result publish failed: %v", err)
		}
		return requestID, err
	}
	return requestID, nil
}
