package mqtt

import (
	"fmt"
	"time"

	"unifix.local/mock/internal/identity"
	"unifix.local/mock/internal/state"
)

// Saison-9-pcap-verified heartbeat wire format.
//
// Outer wrapper:
//
//	0x12 + varint(body_len) + body
//
// Body:
//
//	0x0a + len + mock_id      (Lead-Feld 1, identity.ID)
//	0x12 + len + model        (Lead-Feld 2, identity.Model)
//	0x1a + len + name         (Lead-Feld 3, identity.Name)
//	(0x22 + len + key=value)  x 21   tagged-strings, fixed order
//
// The "adopted=true" string is critical; UDM marks the device
// offline within 60 seconds if it is missing.
const (
	leadTag1     byte = 0x0a // identity.ID
	leadTag2     byte = 0x12 // identity.Model
	leadTag3     byte = 0x1a // identity.Name
	stringTag    byte = 0x22 // each tagged-string entry
	outerWrapper byte = 0x12 // wraps the whole body
)

// heartbeatStringFields holds the saison-9 21-string-entry order.
// Order MUST match the pcap exactly; reordering changes the byte
// stream UDM compares against.
func heartbeatStringFields(
	id *identity.MockIdentity,
	bundle *state.Bundle,
	boot bootState,
) []string {
	now := time.Now().UTC()
	timestamp := now.Format("2006-01-02 15:04:05") + " +0000 CST"
	uptime := now.Unix() - boot.startTime

	return []string{
		"adopted=true",
		"revision=",
		fmt.Sprintf("fw=%s", id.Firmware),
		fmt.Sprintf("name=%s", id.Name),
		"company_id=",
		fmt.Sprintf("controller_id=%s", bundle.ControllerID),
		fmt.Sprintf("location_id=%s", bundle.LocationID),
		"proto_version=1",
		fmt.Sprintf("app_ver=%s", id.AppVersion),
		fmt.Sprintf("mac=%s", id.MAC.String()),
		fmt.Sprintf("port=%d", id.ServicePort),
		fmt.Sprintf("ipv4=%s", id.IPv4.String()),
		fmt.Sprintf("timestamp=%s", timestamp),
		"firmware_id=",
		"mode=user",
		"uah_id=",
		fmt.Sprintf("hw_type=%s", id.HWType),
		fmt.Sprintf("uptime=%d", uptime),
		"security_check=false",
		fmt.Sprintf("start_time=%d", boot.startTime),
		fmt.Sprintf("guid=%s", boot.bootGUID),
	}
}

// buildHeartbeatBody constructs one full /stat-topic payload.
func buildHeartbeatBody(
	id *identity.MockIdentity,
	bundle *state.Bundle,
	boot bootState,
) []byte {
	body := make([]byte, 0, 512)
	body = appendLengthDelim(body, leadTag1, []byte(id.ID))
	body = appendLengthDelim(body, leadTag2, []byte(id.Model))
	body = appendLengthDelim(body, leadTag3, []byte(id.Name))
	for _, kv := range heartbeatStringFields(id, bundle, boot) {
		body = appendLengthDelim(body, stringTag, []byte(kv))
	}
	out := make([]byte, 0, len(body)+8)
	out = append(out, outerWrapper)
	out = appendVarint(out, uint64(len(body)))
	out = append(out, body...)
	return out
}

func appendLengthDelim(dst []byte, tag byte, payload []byte) []byte {
	dst = append(dst, tag)
	dst = appendVarint(dst, uint64(len(payload)))
	dst = append(dst, payload...)
	return dst
}

func appendVarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}
