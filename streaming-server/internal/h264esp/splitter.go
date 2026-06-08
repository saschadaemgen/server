package h264esp

import (
	"errors"
	"fmt"
	"io"
)

// AUSplitter reads a concatenated H.264 Annex-B byte stream from r and
// emits one complete Access Unit per [AUSplitter.Next] call. The
// emitted AU keeps Annex-B framing (each NAL prefixed with a 4-byte
// `00 00 00 01` start code) so the ESP can feed it straight to its
// software decoder.
//
// Briefing-pinned shape (S6-02):
//
//   - Keyframe AU = SPS + PPS + IDR (+ optional SEI) coalesced into
//     one AU. Late-joiners can decode from the first AU they see.
//   - P-frame AU = a single non-IDR slice NAL.
//   - One slice per frame — multi-slice is a later VERIFY from the
//     ESP-chat; for now any sequence of slices in a row would be
//     treated as separate AUs (which still decodes, just with extra
//     boundary noise; not a regression for the single-slice mode we
//     emit).
//
// NAL boundary detection follows the standard Annex-B start-code
// scan, accepting both 3-byte (`00 00 01`) and 4-byte (`00 00 00 01`)
// prefixes on input. The output always uses 4-byte prefixes so the
// ESP sees a uniform stream.
//
// AU boundary detection is purely structural: every VCL slice NAL
// (type 1 = non-IDR, type 5 = IDR) closes the current AU and starts
// the next one. Non-VCL NALs that arrive before a slice (SPS=7,
// PPS=8, SEI=6) are buffered into the current AU as its prefix —
// exactly the shape the briefing demands for keyframes.
//
// The splitter does NOT attempt to interpret first_mb_in_slice (the
// canonical way to detect a "new frame" inside a single slice
// sequence), because libx264 with `-refs 1 -bf 0` and one slice per
// frame produces exactly one VCL NAL per AU — there's no ambiguity
// to resolve.
type AUSplitter struct {
	r   io.Reader
	buf []byte // accumulator for raw bytes from r

	// pendingNALs collects NAL bytes (WITHOUT start codes) of the AU
	// currently under construction. We re-emit each NAL with a 4-byte
	// start code on flush so the output is always uniform.
	pendingNALs [][]byte
	// pendingHasVCL is true if any of pendingNALs is a VCL slice
	// (type 1 or 5). Used to decide whether the AU is complete once
	// we have a flush trigger.
	pendingHasVCL bool
	// pendingAU is an already-assembled AU stashed by
	// flushTrailingNALs when the trailing-NAL was a slice that closed
	// an AU. takePendingAU drains it on the next Next() call.
	pendingAU []byte

	eof bool // underlying reader is exhausted
}

// NAL-unit type values (low 5 bits of the first NAL header byte).
// Only the ones we actually branch on are named here; the rest pass
// through transparently.
const (
	nalUnitTypeNonIDRSlice = 1
	nalUnitTypeIDRSlice    = 5
	nalUnitTypeSEI         = 6
	nalUnitTypeSPS         = 7
	nalUnitTypePPS         = 8
	nalUnitTypeAUD         = 9
)

// Buffer growth bounds. The initial size is generous enough that a
// typical keyframe AU fits without re-allocation; the max guards
// against runaway growth on malformed input.
const (
	auSplitterInitialBuf = 128 * 1024
	auSplitterMaxBuf     = 16 * 1024 * 1024
)

// ErrAUTooLarge is returned when buffered bytes exceed
// [auSplitterMaxBuf] without yielding a complete AU.
var ErrAUTooLarge = errors.New("h264esp: buffered AU exceeds maximum size")

// NewAUSplitter wraps r. Reads happen lazily on each [AUSplitter.Next]
// call.
func NewAUSplitter(r io.Reader) *AUSplitter {
	return &AUSplitter{
		r:           r,
		buf:         make([]byte, 0, auSplitterInitialBuf),
		pendingNALs: make([][]byte, 0, 8),
	}
}

// Next returns the next complete Access Unit as an Annex-B byte slice
// (each NAL prefixed with a 4-byte start code). Returns (nil, io.EOF)
// when the underlying stream is exhausted AND no NALs remain pending.
//
// The returned slice is freshly allocated; callers may retain it past
// further Next calls.
func (s *AUSplitter) Next() ([]byte, error) {
	for {
		// Phase 1: try to extract NAL units from whatever's already
		// buffered. emitNALs runs until the buffer has at most one
		// candidate NAL left (we can't know where it ends until we
		// see the next start code or EOF).
		if au, ok := s.tryEmitAU(); ok {
			return au, nil
		}

		if s.eof {
			// Stream is over. Flush whatever NALs remain.
			s.flushTrailingNALs()
			if au, ok := s.takePendingAU(); ok {
				return au, nil
			}
			return nil, io.EOF
		}

		// Phase 2: pull more bytes from the reader.
		if err := s.readMore(); err != nil {
			if errors.Is(err, io.EOF) {
				s.eof = true
				continue
			}
			return nil, err
		}
	}
}

// tryEmitAU scans s.buf for complete NALs (a NAL is complete when the
// next start code is found), feeds them into the pending-AU state
// machine, and returns an AU if one was completed.
//
// The contract: on return, every byte up to the last detected start
// code's beginning has been consumed; the leftover in s.buf is at
// most the start of the NEXT NAL plus its in-flight payload (no
// trailing start code yet).
func (s *AUSplitter) tryEmitAU() ([]byte, bool) {
	for {
		// Find the first start code in s.buf.
		first, firstLen := findStartCode(s.buf, 0)
		if first < 0 {
			// No start code yet. Trim leading garbage (anything that
			// can't possibly be the prefix of a start code: keep at
			// most the last 3 bytes).
			if len(s.buf) > 3 {
				s.buf = append(s.buf[:0], s.buf[len(s.buf)-3:]...)
			}
			return nil, false
		}
		// Discard any garbage before the first start code.
		if first > 0 {
			s.buf = s.buf[first:]
			first = 0
		}

		// Find the NEXT start code to bound the current NAL.
		next, _ := findStartCode(s.buf, firstLen)
		if next < 0 {
			// No second start code yet — the current NAL is still
			// being read.
			return nil, false
		}

		// NAL payload is s.buf[firstLen:next].
		nalBytes := s.buf[firstLen:next]
		if len(nalBytes) == 0 {
			// Two start codes back-to-back; pathological but defensive.
			s.buf = s.buf[next:]
			continue
		}
		nalCopy := make([]byte, len(nalBytes))
		copy(nalCopy, nalBytes)
		if au, ok := s.feedNAL(nalCopy); ok {
			s.buf = s.buf[next:]
			return au, true
		}
		// No AU yet; advance past the consumed NAL and keep scanning.
		s.buf = s.buf[next:]
	}
}

// feedNAL appends nal to the pending AU and returns a completed AU
// if this NAL closes one.
//
// State machine (single-slice-per-frame assumption from the briefing):
//
//   - non-VCL (SPS/PPS/SEI/AUD/etc.) NAL with pendingHasVCL=false:
//     buffer it as AU prefix. (Typical pre-keyframe sequence: SEI,
//     SPS, PPS.)
//   - non-VCL NAL with pendingHasVCL=true: the previous AU ended
//     with a slice and this new SPS/PPS belongs to the NEXT AU.
//     Emit the previous AU, start fresh, then buffer this NAL.
//   - VCL slice NAL: append to current AU and emit immediately.
//     (One slice per frame; the slice is the AU's tail.)
//
// AUDs (type 9) are stripped to match the briefing's wire shape:
// we never want to ship an AUD to the ESP, even if a future ffmpeg
// version starts inserting them.
func (s *AUSplitter) feedNAL(nal []byte) ([]byte, bool) {
	if len(nal) == 0 {
		return nil, false
	}
	nalType := nal[0] & 0x1F

	// AUDs are stripped per S6-02 briefing.
	if nalType == nalUnitTypeAUD {
		return nil, false
	}

	if isVCL(nalType) {
		// Slice closes the AU.
		s.pendingNALs = append(s.pendingNALs, nal)
		s.pendingHasVCL = true
		au := s.assembleAU(s.pendingNALs)
		s.resetPending()
		return au, true
	}

	// Non-VCL NAL.
	if s.pendingHasVCL {
		// A slice already closed the previous AU's tail, but we
		// emitted that one above. This branch shouldn't be reachable
		// because we reset after every emit — keep as defensive
		// fallback: emit the buffered prefix as a (header-only) AU,
		// then start fresh.
		au := s.assembleAU(s.pendingNALs)
		s.resetPending()
		s.pendingNALs = append(s.pendingNALs, nal)
		return au, true
	}

	s.pendingNALs = append(s.pendingNALs, nal)
	return nil, false
}

// flushTrailingNALs handles the last NAL when the stream ended without
// a trailing start code. Treat s.buf (whatever is left after the last
// detected start code) as one more NAL.
func (s *AUSplitter) flushTrailingNALs() {
	first, firstLen := findStartCode(s.buf, 0)
	if first < 0 {
		// Discard partial trailing junk.
		s.buf = s.buf[:0]
		return
	}
	nalBytes := s.buf[first+firstLen:]
	if len(nalBytes) == 0 {
		s.buf = s.buf[:0]
		return
	}
	nalCopy := make([]byte, len(nalBytes))
	copy(nalCopy, nalBytes)
	// feedNAL may return an AU; we just discard the return because
	// the caller (Next) will check takePendingAU.
	if au, ok := s.feedNAL(nalCopy); ok {
		// Stash the emitted AU back into pendingNALs as a single
		// "ready" element by re-decoding — but the simpler invariant
		// is that takePendingAU should produce it. We can't easily
		// stash a pre-assembled AU; instead, push the bytes through
		// pendingNALs by saving the slice we already built. To keep
		// flushTrailingNALs honest, we just hand the AU back via the
		// caller's secondary check below: store it on the splitter.
		s.pendingAU = au
	}
	s.buf = s.buf[:0]
}

// takePendingAU returns any AU produced during flush and clears the
// pending state. If no AU is pending but pendingNALs still has
// content (e.g. the stream ended mid-AU with a header-only sequence
// and no slice ever followed), we DO NOT emit it — the ESP would
// receive headers that the decoder cannot do anything useful with on
// their own. Drop quietly.
func (s *AUSplitter) takePendingAU() ([]byte, bool) {
	if s.pendingAU != nil {
		au := s.pendingAU
		s.pendingAU = nil
		s.resetPending()
		return au, true
	}
	// No final slice arrived; drop any header-only remainder.
	s.resetPending()
	return nil, false
}

// assembleAU concatenates the given NALs as Annex-B (each NAL gets a
// 4-byte start code prefix), returning a freshly-allocated slice the
// caller can keep.
func (s *AUSplitter) assembleAU(nals [][]byte) []byte {
	if len(nals) == 0 {
		return nil
	}
	// Output size: 4 bytes prefix per NAL + total NAL payload.
	total := 0
	for _, n := range nals {
		total += 4 + len(n)
	}
	out := make([]byte, 0, total)
	for _, n := range nals {
		out = append(out, 0x00, 0x00, 0x00, 0x01)
		out = append(out, n...)
	}
	return out
}

// resetPending zeros the pending-AU state.
func (s *AUSplitter) resetPending() {
	s.pendingNALs = s.pendingNALs[:0]
	s.pendingHasVCL = false
}

// readMore extends s.buf by reading from the underlying reader.
func (s *AUSplitter) readMore() error {
	if cap(s.buf)-len(s.buf) < 4096 {
		newCap := cap(s.buf) * 2
		if newCap > auSplitterMaxBuf {
			newCap = auSplitterMaxBuf
		}
		if newCap == cap(s.buf) {
			return ErrAUTooLarge
		}
		grown := make([]byte, len(s.buf), newCap)
		copy(grown, s.buf)
		s.buf = grown
	}
	n, err := s.r.Read(s.buf[len(s.buf):cap(s.buf)])
	if n > 0 {
		s.buf = s.buf[:len(s.buf)+n]
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		// data + EOF: tell the caller; we still need to drain s.buf.
		if errors.Is(err, io.EOF) {
			return io.EOF
		}
		return nil
	}
	return err
}

// findStartCode returns the byte index of the next Annex-B start code
// in buf[from:], plus its length (3 or 4). Returns -1, 0 if none.
//
// We look for the 3-byte pattern `00 00 01`; if it's preceded by an
// extra 0x00 we report the 4-byte form so the NAL boundary is correct
// (the extra zero is part of the start code, not the previous NAL).
func findStartCode(buf []byte, from int) (idx int, length int) {
	i := from
	for i+2 < len(buf) {
		if buf[i] != 0x00 {
			i++
			continue
		}
		// buf[i] == 0x00. Match `00 00 01` or `00 00 00 01`.
		if buf[i+1] == 0x00 && buf[i+2] == 0x01 {
			return i, 3
		}
		if buf[i+1] == 0x00 && buf[i+2] == 0x00 && i+3 < len(buf) && buf[i+3] == 0x01 {
			return i, 4
		}
		i++
	}
	return -1, 0
}

// isVCL reports whether nalType is a video coding layer (slice) NAL.
// We treat NAL types 1..5 as VCL: 1 = P-slice, 5 = IDR. 2..4 are
// data-partition slices (extremely rare in modern encoders; libx264
// doesn't emit them, but tolerate them in case some other producer
// shows up).
func isVCL(nalType byte) bool {
	return nalType >= 1 && nalType <= 5
}

// AccessUnitSummary is a debug-only helper that returns a human-
// readable description of an Annex-B AU. Useful in test failures so
// the diff message names the NAL types rather than dumping raw hex.
func AccessUnitSummary(au []byte) string {
	parts := []string{}
	i := 0
	for {
		idx, length := findStartCode(au, i)
		if idx < 0 {
			break
		}
		// Find end of this NAL (next start code or end of buffer).
		next, _ := findStartCode(au, idx+length)
		end := next
		if end < 0 {
			end = len(au)
		}
		if idx+length < end {
			nalType := au[idx+length] & 0x1F
			parts = append(parts, fmt.Sprintf("type=%d(%dB)", nalType, end-(idx+length)))
		}
		if next < 0 {
			break
		}
		i = next
	}
	return fmt.Sprintf("AU{%s}", joinComma(parts))
}

func joinComma(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}
