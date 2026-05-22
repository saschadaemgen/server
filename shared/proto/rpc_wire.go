package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// RPC wire format, reverse-engineered, protobuf-like.
//
// Responses produced by the mock keep the original outer wrapper:
//   0x12 + varint(body_length) + body
//
// UDM-side REQUESTS observed in live captures (/update_tokens,
// /remote_view, /cancel_doorbell_notification) arrive WITHOUT the
// outer wrapper. The whole frame is the body, starting directly
// with the first map-entry tag 0x0a.
//
// Body map entry:   0x0a + length-delim + entry
// Entry:            0x0a + len + key + 0x12 + len + value
//
// DecodeRPCRequest accepts both shapes. EncodeRPCResponse still
// emits the outer wrapper because UDM accepts that form for
// responses and we have no reason to change a working encoder.

// Wire-level key strings used inside the body map. These were
// never pinned down via live capture; the values below are
// placeholders that round-trip cleanly through the helpers below.
// Adjust if a future capture reveals different keys.
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
// (0x12 sub-message with tokens, settings, intercom list) that
// the mock does not need to introspect, only forward as Raw for
// higher-level handlers.
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

// DecodeRPCRequestBody walks the RPC body and produces a flat
// key/value map plus an optional "_submessage" entry.
//
// Two passes run over the body:
//
//  1. A strict-map-entry walker harvests every length-delimited
//     field whose payload looks like
//     0x0a+len+key+0x12+len+value into the top-level out map.
//     This catches path, requestId (top-level), and any nested
//     config strings (d2-01-tagged bundle entries).
//
//  2. An inner-submessage extractor finds the first top-level
//     wire-type-2 field whose payload is NOT a strict map-entry,
//     decodes it as standard protobuf with numbered fields, and
//     stores the result under out["_submessage"]. This exposes
//     /remote_view's UUID, MAC, room-id fields by their wire
//     field numbers (field_1, field_5, field_9 ...).
//
// Outer wrapper (0x12+varint) is skipped if present. Best-effort:
// returns the partial map even on truncated or malformed input.
func DecodeRPCRequestBody(data []byte) (map[string]any, error) {
	out := make(map[string]any)
	if len(data) == 0 {
		return out, nil
	}
	body := data
	if data[0] == rpcOuterTag {
		b, _, err := readLengthDelimited(data[1:])
		if err == nil {
			body = b
		}
	}
	walkProtoFields(body, out)
	if sub := extractInnerSubmessage(body); sub != nil && len(sub) > 0 {
		out["_submessage"] = sub
	}
	return out, nil
}

// extractInnerSubmessage walks the top-level fields of body and
// returns the first non-map-entry wire-type-2 field's payload
// decoded as a standard-protobuf submessage. Returns nil if no
// suitable field is found.
func extractInnerSubmessage(data []byte) map[string]any {
	for len(data) > 0 {
		tag, n := binary.Uvarint(data)
		if n <= 0 {
			return nil
		}
		data = data[n:]
		switch tag & 0x07 {
		case 0:
			_, vn := binary.Uvarint(data)
			if vn <= 0 {
				return nil
			}
			data = data[vn:]
		case 1:
			if len(data) < 8 {
				return nil
			}
			data = data[8:]
		case 5:
			if len(data) < 4 {
				return nil
			}
			data = data[4:]
		case 2:
			length, ln := binary.Uvarint(data)
			if ln <= 0 {
				return nil
			}
			end := ln + int(length)
			if end > len(data) {
				return nil
			}
			payload := data[ln:end]
			data = data[end:]
			if _, _, ok := tryParseMapEntry(payload); ok {
				continue
			}
			sub, _ := DecodeProtobufSubmessage(payload)
			if len(sub) > 0 {
				return sub
			}
		default:
			return nil
		}
	}
	return nil
}

// DecodeProtobufSubmessage parses a standard-protobuf-encoded
// submessage and returns a map keyed by "field_N" where N is the
// wire field number. Wire type 0 (varint) values become int64,
// wire type 2 (length-delimited) values become string. Wire types
// 1 (64-bit) and 5 (32-bit) are length-skipped; unknown or
// deprecated wire types stop the walk.
//
// Repeated occurrences of the same field number coalesce into a
// []any holding all observed values in order.
//
// Best-effort: returns the partial map plus an error on truncated
// or malformed input.
func DecodeProtobufSubmessage(raw []byte) (map[string]any, error) {
	out := make(map[string]any)
	for len(raw) > 0 {
		tag, n := binary.Uvarint(raw)
		if n <= 0 {
			return out, errors.New("proto: truncated tag")
		}
		raw = raw[n:]
		fieldNum := tag >> 3
		switch tag & 0x07 {
		case 0:
			v, vn := binary.Uvarint(raw)
			if vn <= 0 {
				return out, errors.New("proto: truncated varint value")
			}
			raw = raw[vn:]
			addFieldByNumber(out, fieldNum, int64(v))
		case 2:
			length, ln := binary.Uvarint(raw)
			if ln <= 0 {
				return out, errors.New("proto: truncated length")
			}
			end := ln + int(length)
			if end > len(raw) {
				return out, errors.New("proto: length overruns buffer")
			}
			addFieldByNumber(out, fieldNum, string(raw[ln:end]))
			raw = raw[end:]
		case 1:
			if len(raw) < 8 {
				return out, errors.New("proto: truncated 64-bit value")
			}
			raw = raw[8:]
		case 5:
			if len(raw) < 4 {
				return out, errors.New("proto: truncated 32-bit value")
			}
			raw = raw[4:]
		default:
			return out, fmt.Errorf("proto: unsupported wire type %d", tag&0x07)
		}
	}
	return out, nil
}

func addFieldByNumber(out map[string]any, fieldNum uint64, value any) {
	key := fmt.Sprintf("field_%d", fieldNum)
	if existing, exists := out[key]; exists {
		if list, isList := existing.([]any); isList {
			out[key] = append(list, value)
			return
		}
		out[key] = []any{existing, value}
		return
	}
	out[key] = value
}

// walkProtoFields iterates a protobuf wire stream, extracting any
// length-delimited field whose payload matches the strict
// "0x0a + len + key + 0x12 + len + value" shape into out. All
// other length-delimited fields are recursed into; varints,
// fixed32, fixed64 are skipped. Stops silently on parse errors.
func walkProtoFields(data []byte, out map[string]any) {
	for len(data) > 0 {
		tag, n := binary.Uvarint(data)
		if n <= 0 {
			return
		}
		data = data[n:]
		switch tag & 0x07 {
		case 0:
			_, vn := binary.Uvarint(data)
			if vn <= 0 {
				return
			}
			data = data[vn:]
		case 1:
			if len(data) < 8 {
				return
			}
			data = data[8:]
		case 2:
			length, ln := binary.Uvarint(data)
			if ln <= 0 {
				return
			}
			end := ln + int(length)
			if end > len(data) {
				return
			}
			payload := data[ln:end]
			data = data[end:]
			if k, v, ok := tryParseMapEntry(payload); ok {
				out[k] = v
				continue
			}
			walkProtoFields(payload, out)
		case 5:
			if len(data) < 4 {
				return
			}
			data = data[4:]
		default:
			return
		}
	}
}

// tryParseMapEntry returns (key, value, true) iff data is exactly
// a strict map-entry: 0x0a + varint(len) + key + 0x12 +
// varint(len) + value, with no trailing bytes. Multi-field
// records (e.g. method-registry entries with name+path+method)
// are rejected so they get recursed instead.
func tryParseMapEntry(data []byte) (key, value string, ok bool) {
	if len(data) < 4 || data[0] != rpcEntryKeyTag {
		return "", "", false
	}
	keyLen, n := binary.Uvarint(data[1:])
	if n <= 0 {
		return "", "", false
	}
	keyStart := 1 + n
	keyEnd := keyStart + int(keyLen)
	if keyEnd >= len(data) || data[keyEnd] != rpcEntryValTag {
		return "", "", false
	}
	valLen, n2 := binary.Uvarint(data[keyEnd+1:])
	if n2 <= 0 {
		return "", "", false
	}
	valStart := keyEnd + 1 + n2
	valEnd := valStart + int(valLen)
	if valEnd != len(data) {
		return "", "", false
	}
	return string(data[keyStart:keyEnd]), string(data[valStart:valEnd]), true
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
