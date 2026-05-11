package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// RPC wire format (saison 8 reverse engineering, protobuf-like).
//
// Responses produced by the mock keep the saison-9 outer wrapper:
//   0x12 + varint(body_length) + body
//
// UDM-side REQUESTS observed in saison 10 (live capture of
// /update_tokens, /remote_view, /cancel_doorbell_notification)
// arrive WITHOUT the outer wrapper. The whole frame is the body,
// starting directly with the first map-entry tag 0x0a.
//
// Body map entry:   0x0a + length-delim + entry
// Entry:            0x0a + len + key + 0x12 + len + value
//
// DecodeRPCRequest accepts both shapes. EncodeRPCResponse still
// emits the outer wrapper because UDM accepts that form for
// responses and we have no reason to change a working encoder.

// Wire-level key strings used inside the body map. Saison 9 did not
// pin these down via live capture; the values below are placeholders
// that round-trip cleanly through the helpers below. Adjust if a
// future capture reveals different keys.
const (
	rpcKeyPath      = "path"
	rpcKeyRequestID = "requestId"
	rpcKeyStatus    = "status"
)

const (
	rpcOuterTag    byte = 0x12 // outer wrapper, field 2 wire-type 2
	rpcMapEntryTag byte = 0x0a // map entry, field 1 wire-type 2
	rpcEntryKeyTag byte = 0x0a // entry's key, field 1 wire-type 2
	rpcEntryValTag byte = 0x12 // entry's value, field 2 wire-type 2
)

// RPCRequest captures the path, request id, and raw bytes of an
// incoming RPC request decoded from the wire format.
type RPCRequest struct {
	Path      string
	RequestID string
	Raw       []byte // full body for handlers that need more
}

// EncodeRPCResponse builds a wire-format response with the given
// path and an optional outer status string (e.g. "success").
// Empty fields are skipped.
func EncodeRPCResponse(path string, requestID string, status string) []byte {
	var body []byte
	if path != "" {
		body = append(body, encodeMapEntry(rpcKeyPath, path)...)
	}
	if requestID != "" {
		body = append(body, encodeMapEntry(rpcKeyRequestID, requestID)...)
	}
	if status != "" {
		body = append(body, encodeMapEntry(rpcKeyStatus, status)...)
	}
	return encodeLengthDelimited(rpcOuterTag, body)
}

// DecodeRPCRequest parses an incoming RPC request and extracts
// the path and known map entries (requestId, etc.).
//
// Two wire shapes are accepted:
//   - Legacy / response shape: 0x12 + varint(len) + body
//   - UDM-native request shape: body starts directly with 0x0a
//
// Any other lead byte is rejected so the caller's hex+ascii
// diagnostics still trigger on truly unexpected payloads.
func DecodeRPCRequest(data []byte) (*RPCRequest, error) {
	if len(data) == 0 {
		return nil, errors.New("proto: empty rpc data")
	}
	switch data[0] {
	case rpcOuterTag:
		body, _, err := readLengthDelimited(data[1:])
		if err != nil {
			return nil, fmt.Errorf("proto: outer body: %w", err)
		}
		return decodeRequestBody(body, data)
	case rpcMapEntryTag:
		return decodeRequestBody(data, data)
	default:
		return nil, fmt.Errorf("proto: unexpected lead byte 0x%02x", data[0])
	}
}

// decodeRequestBody walks the leading 0x0a map-entry blocks and
// pulls out path + request id. Anything past the first non-map-
// entry tag is ignored: real UDM requests carry additional fields
// (0x12 sub-message with tokens, settings, intercom list) that the
// saison-10/11 mock does not need to introspect, only forward as
// Raw for higher-level handlers.
func decodeRequestBody(body, raw []byte) (*RPCRequest, error) {
	req := &RPCRequest{Raw: append([]byte(nil), raw...)}
	for len(body) > 0 {
		if body[0] != rpcMapEntryTag {
			break
		}
		entry, consumed, err := readLengthDelimited(body[1:])
		if err != nil {
			return nil, fmt.Errorf("proto: map entry: %w", err)
		}
		key, value, err := decodeMapEntry(entry)
		if err != nil {
			return nil, err
		}
		switch key {
		case rpcKeyPath:
			req.Path = value
		case rpcKeyRequestID:
			req.RequestID = value
		}
		body = body[1+consumed:]
	}
	return req, nil
}

func encodeMapEntry(key, value string) []byte {
	inner := encodeLengthDelimited(rpcEntryKeyTag, []byte(key))
	inner = append(inner, encodeLengthDelimited(rpcEntryValTag, []byte(value))...)
	return encodeLengthDelimited(rpcMapEntryTag, inner)
}

func decodeMapEntry(entry []byte) (key, value string, err error) {
	if len(entry) == 0 || entry[0] != rpcEntryKeyTag {
		return "", "", fmt.Errorf("proto: missing entry key tag 0x%02x", rpcEntryKeyTag)
	}
	keyBytes, consumed, err := readLengthDelimited(entry[1:])
	if err != nil {
		return "", "", fmt.Errorf("proto: entry key: %w", err)
	}
	rest := entry[1+consumed:]
	if len(rest) == 0 || rest[0] != rpcEntryValTag {
		return "", "", fmt.Errorf("proto: missing entry value tag 0x%02x", rpcEntryValTag)
	}
	valBytes, _, err := readLengthDelimited(rest[1:])
	if err != nil {
		return "", "", fmt.Errorf("proto: entry value: %w", err)
	}
	return string(keyBytes), string(valBytes), nil
}

// encodeLengthDelimited writes one tag-prefixed, varint-length-
// delimited byte slice.
func encodeLengthDelimited(tag byte, payload []byte) []byte {
	out := make([]byte, 0, 1+binary.MaxVarintLen64+len(payload))
	out = append(out, tag)
	var lb [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(lb[:], uint64(len(payload)))
	out = append(out, lb[:n]...)
	out = append(out, payload...)
	return out
}

// readLengthDelimited reads a varint length from data, then that
// many payload bytes. Returns the payload plus the total bytes
// consumed from data (varint length plus payload).
func readLengthDelimited(data []byte) (payload []byte, consumed int, err error) {
	v, n := binary.Uvarint(data)
	if n == 0 {
		return nil, 0, errors.New("proto: truncated varint length")
	}
	if n < 0 {
		return nil, 0, errors.New("proto: varint length overflow")
	}
	end := n + int(v)
	if end > len(data) {
		return nil, 0, fmt.Errorf("proto: length %d overruns buffer (have %d)", v, len(data)-n)
	}
	return data[n:end], end, nil
}
