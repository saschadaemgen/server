package h264esp

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// --- low-level helpers ------------------------------------------------------

// nal builds a fake NAL byte slice with the given NAL type. The
// payload is meaningless filler (just enough bytes to make the
// matching unambiguous in failure messages).
func nal(typ byte, n int) []byte {
	b := make([]byte, n)
	b[0] = typ & 0x1F // forbidden_zero_bit=0, nal_ref_idc=0, type=typ
	for i := 1; i < n; i++ {
		b[i] = byte('a' + (i % 26))
	}
	return b
}

// annexB concatenates NALs with 4-byte start codes (the canonical
// shape ffmpeg writes).
func annexB(nals ...[]byte) []byte {
	var buf bytes.Buffer
	for _, n := range nals {
		buf.Write([]byte{0x00, 0x00, 0x00, 0x01})
		buf.Write(n)
	}
	return buf.Bytes()
}

// annexB3 concatenates NALs with 3-byte start codes (also legal per
// spec; ffmpeg occasionally emits these between NALs after the first).
func annexB3(nals ...[]byte) []byte {
	var buf bytes.Buffer
	for _, n := range nals {
		buf.Write([]byte{0x00, 0x00, 0x01})
		buf.Write(n)
	}
	return buf.Bytes()
}

// drainAUs runs the splitter to completion and returns the AUs.
func drainAUs(t *testing.T, r io.Reader) [][]byte {
	t.Helper()
	sp := NewAUSplitter(r)
	var out [][]byte
	for {
		au, err := sp.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out
			}
			t.Fatalf("Next: %v", err)
		}
		out = append(out, au)
	}
}

// auNALTypes returns the sequence of NAL types found in an emitted
// Annex-B AU. Used in assertions ("the keyframe AU should contain
// SPS, PPS, IDR in that order").
func auNALTypes(au []byte) []byte {
	var out []byte
	i := 0
	for i < len(au) {
		idx, length := findStartCode(au, i)
		if idx < 0 {
			break
		}
		if idx+length >= len(au) {
			break
		}
		nalType := au[idx+length] & 0x1F
		out = append(out, nalType)
		nxt, _ := findStartCode(au, idx+length)
		if nxt < 0 {
			break
		}
		i = nxt
	}
	return out
}

// --- happy-path tests -------------------------------------------------------

func TestSplitter_SingleSlice(t *testing.T) {
	stream := annexB(nal(nalUnitTypeNonIDRSlice, 20))
	aus := drainAUs(t, bytes.NewReader(stream))
	if len(aus) != 1 {
		t.Fatalf("got %d AUs, want 1: %s", len(aus), summaries(aus))
	}
	if got := auNALTypes(aus[0]); !bytesEq(got, []byte{nalUnitTypeNonIDRSlice}) {
		t.Errorf("AU NAL types = %v, want [1]", got)
	}
}

func TestSplitter_KeyframeBundlesSPSPPSIDR(t *testing.T) {
	// THE briefing requirement: a keyframe AU = SPS + PPS + IDR in one
	// chunk so the ESP can decode from a late-join.
	stream := annexB(
		nal(nalUnitTypeSPS, 30),
		nal(nalUnitTypePPS, 10),
		nal(nalUnitTypeIDRSlice, 5000),
	)
	aus := drainAUs(t, bytes.NewReader(stream))
	if len(aus) != 1 {
		t.Fatalf("got %d AUs, want 1: %s", len(aus), summaries(aus))
	}
	got := auNALTypes(aus[0])
	want := []byte{nalUnitTypeSPS, nalUnitTypePPS, nalUnitTypeIDRSlice}
	if !bytesEq(got, want) {
		t.Errorf("keyframe AU NAL types = %v, want %v", got, want)
	}
}

func TestSplitter_KeyframePlusPFrames(t *testing.T) {
	// One keyframe followed by N P-frames is the typical GoP shape.
	// We expect: AU#0 = SPS+PPS+IDR, AU#1..N = single non-IDR slice.
	stream := annexB(
		nal(nalUnitTypeSPS, 30),
		nal(nalUnitTypePPS, 10),
		nal(nalUnitTypeIDRSlice, 5000),
		nal(nalUnitTypeNonIDRSlice, 800),
		nal(nalUnitTypeNonIDRSlice, 750),
		nal(nalUnitTypeNonIDRSlice, 900),
	)
	aus := drainAUs(t, bytes.NewReader(stream))
	if len(aus) != 4 {
		t.Fatalf("got %d AUs, want 4 (key + 3 P): %s", len(aus), summaries(aus))
	}
	if !bytesEq(auNALTypes(aus[0]), []byte{nalUnitTypeSPS, nalUnitTypePPS, nalUnitTypeIDRSlice}) {
		t.Errorf("aus[0] = %v, want SPS+PPS+IDR", auNALTypes(aus[0]))
	}
	for i := 1; i < 4; i++ {
		if !bytesEq(auNALTypes(aus[i]), []byte{nalUnitTypeNonIDRSlice}) {
			t.Errorf("aus[%d] = %v, want single non-IDR slice", i, auNALTypes(aus[i]))
		}
	}
}

func TestSplitter_RepeatedKeyframes(t *testing.T) {
	// SPS+PPS repeated before every IDR is the in-band-headers shape
	// the briefing demands. Verify the splitter coalesces each
	// SPS+PPS+IDR triple into its own AU and the P-frames stand alone.
	stream := annexB(
		// Keyframe 1
		nal(nalUnitTypeSPS, 30), nal(nalUnitTypePPS, 10), nal(nalUnitTypeIDRSlice, 4000),
		// P-frames
		nal(nalUnitTypeNonIDRSlice, 600),
		nal(nalUnitTypeNonIDRSlice, 600),
		// Keyframe 2 (in-band SPS/PPS again)
		nal(nalUnitTypeSPS, 30), nal(nalUnitTypePPS, 10), nal(nalUnitTypeIDRSlice, 4500),
		// More P
		nal(nalUnitTypeNonIDRSlice, 600),
	)
	aus := drainAUs(t, bytes.NewReader(stream))
	if len(aus) != 5 {
		t.Fatalf("got %d AUs, want 5: %s", len(aus), summaries(aus))
	}
	if !bytesEq(auNALTypes(aus[0]), []byte{nalUnitTypeSPS, nalUnitTypePPS, nalUnitTypeIDRSlice}) {
		t.Errorf("aus[0]: %v", auNALTypes(aus[0]))
	}
	if !bytesEq(auNALTypes(aus[3]), []byte{nalUnitTypeSPS, nalUnitTypePPS, nalUnitTypeIDRSlice}) {
		t.Errorf("aus[3]: %v", auNALTypes(aus[3]))
	}
}

func TestSplitter_SEIBeforeKeyframeStaysInside(t *testing.T) {
	// ffmpeg occasionally writes a SEI (NAL type 6) before SPS. The
	// briefing wants the WHOLE pre-slice prefix in the keyframe's AU.
	stream := annexB(
		nal(nalUnitTypeSEI, 50),
		nal(nalUnitTypeSPS, 30),
		nal(nalUnitTypePPS, 10),
		nal(nalUnitTypeIDRSlice, 4000),
	)
	aus := drainAUs(t, bytes.NewReader(stream))
	if len(aus) != 1 {
		t.Fatalf("got %d AUs, want 1: %s", len(aus), summaries(aus))
	}
	want := []byte{nalUnitTypeSEI, nalUnitTypeSPS, nalUnitTypePPS, nalUnitTypeIDRSlice}
	if !bytesEq(auNALTypes(aus[0]), want) {
		t.Errorf("AU NAL types = %v, want %v", auNALTypes(aus[0]), want)
	}
}

// --- start-code variants ----------------------------------------------------

func TestSplitter_Accepts3ByteStartCodes(t *testing.T) {
	// Decoders MUST accept both 3-byte and 4-byte start codes; ffmpeg
	// often emits 3-byte codes for intra-stream NALs after the first.
	stream := annexB3(
		nal(nalUnitTypeSPS, 30),
		nal(nalUnitTypePPS, 10),
		nal(nalUnitTypeIDRSlice, 4000),
	)
	aus := drainAUs(t, bytes.NewReader(stream))
	if len(aus) != 1 {
		t.Fatalf("got %d AUs, want 1: %s", len(aus), summaries(aus))
	}
}

func TestSplitter_OutputAlwaysUses4ByteStartCodes(t *testing.T) {
	// Briefing: the ESP gets a uniform stream. Even if the input mixes
	// 3- and 4-byte start codes, our output always uses 4-byte.
	stream := annexB3(nal(nalUnitTypeNonIDRSlice, 100))
	aus := drainAUs(t, bytes.NewReader(stream))
	if len(aus) != 1 {
		t.Fatalf("got %d AUs", len(aus))
	}
	if !bytes.HasPrefix(aus[0], []byte{0x00, 0x00, 0x00, 0x01}) {
		t.Errorf("AU does not start with 4-byte start code: % x", aus[0][:8])
	}
}

func TestSplitter_StripsAUDs(t *testing.T) {
	// Briefing: NO AUDs in the output stream. If ffmpeg ever inserts
	// them (we don't ask for them, but defend), they must NOT appear
	// in the emitted AU.
	stream := annexB(
		nal(nalUnitTypeAUD, 2),
		nal(nalUnitTypeSPS, 30),
		nal(nalUnitTypePPS, 10),
		nal(nalUnitTypeIDRSlice, 4000),
	)
	aus := drainAUs(t, bytes.NewReader(stream))
	if len(aus) != 1 {
		t.Fatalf("got %d AUs, want 1: %s", len(aus), summaries(aus))
	}
	got := auNALTypes(aus[0])
	for _, typ := range got {
		if typ == nalUnitTypeAUD {
			t.Errorf("AU contains AUD (type 9): %v — must be stripped", got)
		}
	}
	// SPS/PPS/IDR should still be present.
	want := []byte{nalUnitTypeSPS, nalUnitTypePPS, nalUnitTypeIDRSlice}
	if !bytesEq(got, want) {
		t.Errorf("AU types = %v, want %v (AUD stripped)", got, want)
	}
}

// --- streaming / partial-read tests -----------------------------------------

func TestSplitter_HandlesByteAtATimeReader(t *testing.T) {
	// Simulate a slow underlying reader by returning 1 byte per Read.
	// The splitter MUST behave identically regardless of read size —
	// the production reader (an ffmpeg pipe) returns whatever chunks
	// the kernel buffers.
	stream := annexB(
		nal(nalUnitTypeSPS, 30),
		nal(nalUnitTypePPS, 10),
		nal(nalUnitTypeIDRSlice, 200),
		nal(nalUnitTypeNonIDRSlice, 100),
	)
	r := &slowReader{src: stream}
	aus := drainAUs(t, r)
	if len(aus) != 2 {
		t.Fatalf("got %d AUs from byte-at-a-time reader, want 2: %s", len(aus), summaries(aus))
	}
}

func TestSplitter_DiscardsGarbageBeforeFirstStartCode(t *testing.T) {
	// An ffmpeg variant might write banner bytes before the first
	// start code. Tolerate them silently.
	var buf bytes.Buffer
	buf.WriteString("garbage before stream\n")
	buf.Write(annexB(nal(nalUnitTypeNonIDRSlice, 50)))
	aus := drainAUs(t, &buf)
	if len(aus) != 1 {
		t.Fatalf("garbage tolerance broke: got %d AUs", len(aus))
	}
}

// --- EOF handling -----------------------------------------------------------

func TestSplitter_EOFAfterCleanAU(t *testing.T) {
	stream := annexB(nal(nalUnitTypeNonIDRSlice, 80))
	aus := drainAUs(t, bytes.NewReader(stream))
	if len(aus) != 1 {
		t.Fatalf("got %d AUs, want 1", len(aus))
	}
}

func TestSplitter_TrailingNALEmittedOnEOF(t *testing.T) {
	// EOF arrives without a trailing start code. The last NAL must
	// still surface as an AU.
	stream := annexB(
		nal(nalUnitTypeSPS, 30),
		nal(nalUnitTypePPS, 10),
		nal(nalUnitTypeIDRSlice, 4000),
		nal(nalUnitTypeNonIDRSlice, 800),
	)
	aus := drainAUs(t, bytes.NewReader(stream))
	if len(aus) != 2 {
		t.Fatalf("got %d AUs (EOF flush failed): %s", len(aus), summaries(aus))
	}
	if !bytesEq(auNALTypes(aus[1]), []byte{nalUnitTypeNonIDRSlice}) {
		t.Errorf("aus[1] = %v, want [1]", auNALTypes(aus[1]))
	}
}

func TestSplitter_HeaderOnlyTrailingDroppedOnEOF(t *testing.T) {
	// Stream ends with SPS/PPS but no slice. These on their own can't
	// decode anything — the splitter must NOT ship a header-only AU.
	stream := annexB(
		nal(nalUnitTypeNonIDRSlice, 100),
		nal(nalUnitTypeSPS, 30),
		nal(nalUnitTypePPS, 10),
	)
	aus := drainAUs(t, bytes.NewReader(stream))
	if len(aus) != 1 {
		t.Fatalf("got %d AUs, want exactly 1 (slice) — header-only must be dropped: %s", len(aus), summaries(aus))
	}
}

// --- error paths ------------------------------------------------------------

func TestSplitter_ReaderError(t *testing.T) {
	// A non-EOF reader error should surface as-is.
	r := &errorReader{err: errors.New("synthetic read error")}
	sp := NewAUSplitter(r)
	_, err := sp.Next()
	if err == nil || !strings.Contains(err.Error(), "synthetic") {
		t.Errorf("err = %v, want a synthetic-read-error", err)
	}
}

// --- helpers / fakes --------------------------------------------------------

type slowReader struct {
	src []byte
	pos int
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.pos >= len(s.src) {
		return 0, io.EOF
	}
	p[0] = s.src[s.pos]
	s.pos++
	return 1, nil
}

type errorReader struct{ err error }

func (e *errorReader) Read(_ []byte) (int, error) { return 0, e.err }

func bytesEq(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func summaries(aus [][]byte) string {
	parts := make([]string, len(aus))
	for i, au := range aus {
		parts[i] = AccessUnitSummary(au)
	}
	return strings.Join(parts, " | ")
}
