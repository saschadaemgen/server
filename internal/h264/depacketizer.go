// Package h264 provides a tolerant RFC-6184 RTP depacketizer for H.264.
//
// It exists because gortsplib v5 — and pion's H.264 helpers — implement
// only the non-interleaved subset of RFC 6184 (Single NAL, STAP-A, FU-A).
// The UniFi Intercom emits the full spectrum (FU-B, STAP-B, MTAP-16,
// MTAP-24) and additionally pads STAP-A entries in a way gortsplib treats
// as malformed. This package handles all six packetization types and is
// deliberately lenient about real-world encoder quirks.
//
// Decoding-order fields (DON, DONB, DOND) and per-NAL timestamp offsets
// in MTAPs are parsed but discarded: for live single-stream playback the
// RTP timestamp on the carrying packet is sufficient ordering. The DON
// machinery is only meaningful in interleaved transmission of multiple
// streams, which CARVILON does not do.
//
// The depacketizer is transport-agnostic: it takes ([]byte payload, uint16
// sequence number) in and gives [][]byte (NAL units) out, with
// [ErrIncomplete] returned mid-fragmentation. Access-unit assembly
// (grouping NALs by RTP timestamp / marker bit) is the caller's job.
package h264

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// RFC 6184 §5.4 NAL unit type values used as packetization indicators.
const (
	naluTypeMinSingle = 1
	naluTypeMaxSingle = 23
	naluTypeSTAPA     = 24
	naluTypeSTAPB     = 25
	naluTypeMTAP16    = 26
	naluTypeMTAP24    = 27
	naluTypeFUA       = 28
	naluTypeFUB       = 29
)

// Bit masks for the NAL header byte: F(1)|NRI(2)|Type(5).
const (
	naluTypeMask = 0x1F // bits 4..0
	naluFNRIMask = 0xE0 // bits 7..5 (F + NRI)
)

// Bit masks for the FU header byte: S(1)|E(1)|R(1)|Type(5).
const (
	fuStartMask = 0x80
	fuEndMask   = 0x40
)

// ErrIncomplete signals that a fragmented NAL (FU-A / FU-B) is mid-flight:
// the depacketizer has stored the fragments so far and is waiting for
// later RTP packets. Callers should treat it as "no data this packet" —
// not as a drop, not as an error to log.
var ErrIncomplete = errors.New("h264: more packets needed")

// Depacketizer turns RTP payloads into the NAL units they carry.
//
// One instance per logical RTP stream. Not safe for concurrent use; the
// expected pattern is "one goroutine pulls packets, one Depacketizer".
//
// State is only kept for fragmentation reassembly (FU-A / FU-B). All other
// packet types are stateless single-packet decodes.
type Depacketizer struct {
	// FU reassembly state.
	fuActive      bool
	fuExpectedSeq uint16
	fuBuf         []byte // reassembled NAL bytes, starting with the reconstructed NAL header
}

// Decode extracts NAL units from one RTP packet's payload.
//
//   - On a single-packet NAL (Single NAL, STAP-*, MTAP-*) it returns one or
//     more NALs and a nil error.
//   - On a fragmentation start or middle packet it returns (nil, ErrIncomplete).
//   - On a fragmentation end packet it returns the fully reassembled NAL.
//   - On a malformed packet it returns (nil, error). Callers should drop the
//     packet and continue; the loop must not die on a single bad packet.
//
// `seq` is the RTP sequence number, needed to detect packet loss inside an
// FU-A/FU-B sequence. The sequence number is taken as uint16 so wrap-around
// works naturally.
func (d *Depacketizer) Decode(seq uint16, payload []byte) ([][]byte, error) {
	if len(payload) == 0 {
		d.resetFU()
		return nil, errors.New("h264: empty payload")
	}

	naluType := payload[0] & naluTypeMask

	switch {
	case naluType >= naluTypeMinSingle && naluType <= naluTypeMaxSingle:
		// Single NAL: the whole payload is one NAL.
		d.resetFU()
		// Copy so the caller can hold the slice past the gortsplib buffer's
		// lifetime. Cheap (one alloc per packet), and avoids spooky-aliasing
		// bugs when the next packet reuses the same backing array.
		nalu := append([]byte(nil), payload...)
		return [][]byte{nalu}, nil

	case naluType == naluTypeSTAPA:
		d.resetFU()
		return parseSTAP(payload[1:], 0)

	case naluType == naluTypeSTAPB:
		d.resetFU()
		// STAP-B = 1B header + 2B DON + entries.
		if len(payload) < 3 {
			return nil, fmt.Errorf("h264: STAP-B payload too short (%d B)", len(payload))
		}
		return parseSTAP(payload[3:], 0)

	case naluType == naluTypeMTAP16:
		d.resetFU()
		return parseMTAP(payload, 2)

	case naluType == naluTypeMTAP24:
		d.resetFU()
		return parseMTAP(payload, 3)

	case naluType == naluTypeFUA:
		return d.parseFU(seq, payload, 0)

	case naluType == naluTypeFUB:
		return d.parseFU(seq, payload, 2)

	default:
		// Reserved (0, 30, 31).
		d.resetFU()
		return nil, fmt.Errorf("h264: reserved NAL type %d", naluType)
	}
}

// parseSTAP parses a STAP-A or STAP-B payload after the type header (and
// after the 2-byte DON of a STAP-B).
//
// donBytes is unused once the caller has already advanced past the DON —
// it stays in the signature for symmetry with parseFU. Always 0 here.
//
// Tolerant about trailing junk: gortsplib rejects a STAP-A as soon as it
// sees a zero-size marker followed by anything other than all-zero padding.
// The UA-Intercom emits non-zero filler in that position. We instead stop
// parsing further entries at the zero-size marker and return whatever NALs
// we successfully collected before it.
func parseSTAP(payload []byte, donBytes int) ([][]byte, error) {
	_ = donBytes // signature symmetry

	var nalus [][]byte
	for len(payload) > 0 {
		if len(payload) < 2 {
			// Truncated entry header. If we got nothing, error; otherwise
			// keep what we have.
			break
		}
		size := int(binary.BigEndian.Uint16(payload[:2]))
		payload = payload[2:]

		if size == 0 {
			// Zero-size marker. RFC 6184 reserves it; gortsplib accepts only
			// all-zero padding after this point. Real encoders (UA) leave
			// non-zero stuffing here. Treat the rest of the packet as junk
			// and stop — what we already extracted is fine.
			break
		}

		if size > len(payload) {
			if len(nalus) == 0 {
				return nil, fmt.Errorf("h264: STAP entry size %d exceeds %d remaining", size, len(payload))
			}
			break
		}

		nalu := append([]byte(nil), payload[:size]...)
		nalus = append(nalus, nalu)
		payload = payload[size:]
	}

	if len(nalus) == 0 {
		return nil, errors.New("h264: STAP packet carried no NAL units")
	}
	return nalus, nil
}

// parseMTAP parses an MTAP-16 or MTAP-24 payload.
//
//   - tsOffsetBytes is 2 for MTAP-16, 3 for MTAP-24.
//   - The MTAP header is 1B type + 2B DONB = 3 bytes.
//   - Each sub-entry: 2B NAL size, 1B DOND, tsOffsetBytes TS-offset,
//     then NAL bytes (size bytes).
//
// We discard DONB / DOND / TS-offset: for live single-source playback the
// RTP-level ordering already gives us what we need, and the per-NAL time
// offsets do not affect what bytes go on the wire to the browser.
func parseMTAP(payload []byte, tsOffsetBytes int) ([][]byte, error) {
	if len(payload) < 3 {
		return nil, fmt.Errorf("h264: MTAP payload too short for header (%d B)", len(payload))
	}
	payload = payload[3:] // skip 1B type + 2B DONB

	perEntryHeader := 2 + 1 + tsOffsetBytes // size + DOND + TS-offset

	var nalus [][]byte
	for len(payload) > 0 {
		if len(payload) < perEntryHeader {
			break
		}
		size := int(binary.BigEndian.Uint16(payload[:2]))
		if size == 0 {
			break
		}

		needed := perEntryHeader + size
		if needed > len(payload) {
			if len(nalus) == 0 {
				return nil, fmt.Errorf("h264: MTAP entry size %d exceeds %d remaining", size, len(payload)-perEntryHeader)
			}
			break
		}

		nalu := append([]byte(nil), payload[perEntryHeader:needed]...)
		nalus = append(nalus, nalu)
		payload = payload[needed:]
	}

	if len(nalus) == 0 {
		return nil, errors.New("h264: MTAP packet carried no NAL units")
	}
	return nalus, nil
}

// parseFU handles a single FU-A or FU-B packet. donBytes is 2 for FU-B
// (DON is present only in the start fragment) and 0 for FU-A.
func (d *Depacketizer) parseFU(seq uint16, payload []byte, donBytes int) ([][]byte, error) {
	if len(payload) < 2 {
		d.resetFU()
		return nil, fmt.Errorf("h264: FU payload too short for indicator+header (%d B)", len(payload))
	}

	fuIndicator := payload[0]
	fuHeader := payload[1]
	start := fuHeader&fuStartMask != 0
	end := fuHeader&fuEndMask != 0
	origType := fuHeader & naluTypeMask

	body := payload[2:]

	if start {
		// FU-B start carries a 2-byte DON between FU header and fragment data.
		if donBytes > 0 {
			if len(body) < donBytes {
				d.resetFU()
				return nil, fmt.Errorf("h264: FU-B start too short for DON (%d B)", len(body))
			}
			body = body[donBytes:]
		}

		d.fuActive = true
		d.fuExpectedSeq = seq + 1 // wraps naturally as uint16
		// Reconstruct the original NAL header: F+NRI from the FU indicator,
		// Type from the FU header.
		origNALHeader := (fuIndicator & naluFNRIMask) | origType
		d.fuBuf = append(d.fuBuf[:0], origNALHeader)
		d.fuBuf = append(d.fuBuf, body...)

		if end {
			// Some encoders emit a single packet with both S=1 and E=1 —
			// RFC 6184 §5.8 forbids it, but it happens in the wild (gortsplib
			// notes the CostarHD case). Treat it as a complete NAL.
			nalu := append([]byte(nil), d.fuBuf...)
			d.resetFU()
			return [][]byte{nalu}, nil
		}
		return nil, ErrIncomplete
	}

	// Middle or end fragment.
	if !d.fuActive {
		// We never saw a start. Could be packet loss at session entry; drop
		// quietly and wait for the next start.
		return nil, ErrIncomplete
	}

	if seq != d.fuExpectedSeq {
		// Lost or reordered packet inside the FU. Abandon this fragment;
		// the next IDR will re-sync.
		d.resetFU()
		return nil, fmt.Errorf("h264: FU sequence gap (expected %d, got %d)", d.fuExpectedSeq, seq)
	}

	d.fuExpectedSeq++
	d.fuBuf = append(d.fuBuf, body...)

	if end {
		nalu := append([]byte(nil), d.fuBuf...)
		d.resetFU()
		return [][]byte{nalu}, nil
	}
	return nil, ErrIncomplete
}

func (d *Depacketizer) resetFU() {
	d.fuActive = false
	d.fuExpectedSeq = 0
	d.fuBuf = d.fuBuf[:0]
}
