package h264

import (
	"bytes"
	"errors"
	"testing"
)

// Helper: build the leading NAL header byte from F/NRI/type.
func nalHeader(f, nri, typ byte) byte {
	return (f << 7) | ((nri & 0x3) << 5) | (typ & 0x1F)
}

// Helper: build an FU header byte from S/E/R/type.
func fuHeader(s, e, r, typ byte) byte {
	var b byte
	if s != 0 {
		b |= 0x80
	}
	if e != 0 {
		b |= 0x40
	}
	if r != 0 {
		b |= 0x20
	}
	return b | (typ & 0x1F)
}

func assertNALsEqual(t *testing.T, got, want [][]byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("nalu count: got %d, want %d (got=%x, want=%x)", len(got), len(want), got, want)
	}
	for i := range got {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("nalu[%d]: got %x, want %x", i, got[i], want[i])
		}
	}
}

// ----- Single NAL (types 1..23) ----------------------------------------------

func TestDecode_SingleNAL_IDR(t *testing.T) {
	d := &Depacketizer{}
	payload := []byte{nalHeader(0, 3, 5), 0xAA, 0xBB, 0xCC} // IDR slice
	got, err := d.Decode(100, payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertNALsEqual(t, got, [][]byte{payload})
}

func TestDecode_SingleNAL_SPS(t *testing.T) {
	d := &Depacketizer{}
	payload := []byte{nalHeader(0, 3, 7), 0x42, 0xE0, 0x1F} // SPS
	got, err := d.Decode(100, payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertNALsEqual(t, got, [][]byte{payload})
}

func TestDecode_SingleNAL_CopyIndependent(t *testing.T) {
	// The depacketizer must not return a slice that aliases the input — the
	// caller will pass us gortsplib's RTP packet buffer, which gets reused.
	d := &Depacketizer{}
	payload := []byte{nalHeader(0, 2, 1), 0x11, 0x22}
	got, err := d.Decode(100, payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	payload[1] = 0xFF // corrupt the source buffer
	if got[0][1] == 0xFF {
		t.Fatalf("NAL aliases input buffer; expected an independent copy")
	}
}

// ----- STAP-A (type 24) -------------------------------------------------------

func TestDecode_STAPA_TwoNALs(t *testing.T) {
	d := &Depacketizer{}
	payload := []byte{
		nalHeader(0, 3, naluTypeSTAPA),
		0x00, 0x04, nalHeader(0, 3, 7), 0x42, 0xE0, 0x1F, // SPS (4 B)
		0x00, 0x02, nalHeader(0, 3, 8), 0x00, /*       */ // PPS (2 B)
	}
	got, err := d.Decode(100, payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := [][]byte{
		{nalHeader(0, 3, 7), 0x42, 0xE0, 0x1F},
		{nalHeader(0, 3, 8), 0x00},
	}
	assertNALsEqual(t, got, want)
}

func TestDecode_STAPA_ToleratesNonZeroPaddingAfterZeroSize(t *testing.T) {
	// UA-Intercom case: a STAP-A ends with a zero-size marker and then has
	// non-null filler bytes. gortsplib rejects this; we must accept the NALs
	// we already extracted.
	d := &Depacketizer{}
	payload := []byte{
		nalHeader(0, 3, naluTypeSTAPA),
		0x00, 0x03, 0x01, 0x02, 0x03, // size=3, 3 NAL bytes
		0x00, 0x00, /*             */ // zero-size marker
		0xCA, 0xFE, 0xBA, 0xBE, /* */ // non-null filler — must NOT error
	}
	got, err := d.Decode(100, payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := [][]byte{{0x01, 0x02, 0x03}}
	assertNALsEqual(t, got, want)
}

func TestDecode_STAPA_MalformedSizeReturnsErrorWhenNothingExtracted(t *testing.T) {
	d := &Depacketizer{}
	payload := []byte{
		nalHeader(0, 3, naluTypeSTAPA),
		0x00, 0xFF, 0x01, 0x02, // declared size 255 but only 2 bytes remain
	}
	_, err := d.Decode(100, payload)
	if err == nil {
		t.Fatal("expected error for STAP-A with bad size")
	}
}

func TestDecode_STAPA_KeepsAlreadyExtractedNALsOnTrailingError(t *testing.T) {
	// First entry good, second entry malformed. We expect the good NAL back
	// without an error — single-bad-packet should not poison a packet that
	// already carried a valid NAL.
	d := &Depacketizer{}
	payload := []byte{
		nalHeader(0, 3, naluTypeSTAPA),
		0x00, 0x02, 0x11, 0x22, // valid entry
		0x00, 0xFF, 0x33, /*  */ // declared size 255, only 1 byte left
	}
	got, err := d.Decode(100, payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertNALsEqual(t, got, [][]byte{{0x11, 0x22}})
}

// ----- STAP-B (type 25) -------------------------------------------------------

func TestDecode_STAPB_WithDON(t *testing.T) {
	d := &Depacketizer{}
	payload := []byte{
		nalHeader(0, 3, naluTypeSTAPB),
		0x12, 0x34, /*             */ // DON
		0x00, 0x03, 0x67, 0x42, 0x1F, // NAL[3]
		0x00, 0x02, 0x68, 0xCE, /* */ // NAL[2]
	}
	got, err := d.Decode(100, payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := [][]byte{
		{0x67, 0x42, 0x1F},
		{0x68, 0xCE},
	}
	assertNALsEqual(t, got, want)
}

// ----- MTAP-16 (type 26) ------------------------------------------------------

func TestDecode_MTAP16_TwoNALs(t *testing.T) {
	d := &Depacketizer{}
	// Entry header per sub-payload: 2B size + 1B DOND + 2B TS-offset = 5 B.
	payload := []byte{
		nalHeader(0, 3, naluTypeMTAP16),
		0x12, 0x34, /*                                                    */ // DONB
		0x00, 0x03, 0x55, 0x00, 0x10, 0x67, 0x42, 0x1F, /*                */ // size=3 NAL[3]
		0x00, 0x02, 0x56, 0x00, 0x20, 0x68, 0xCE, /*                      */ // size=2 NAL[2]
	}
	got, err := d.Decode(100, payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := [][]byte{
		{0x67, 0x42, 0x1F},
		{0x68, 0xCE},
	}
	assertNALsEqual(t, got, want)
}

// ----- MTAP-24 (type 27) ------------------------------------------------------

func TestDecode_MTAP24_TwoNALs(t *testing.T) {
	d := &Depacketizer{}
	// Entry header: 2B size + 1B DOND + 3B TS-offset = 6 B.
	payload := []byte{
		nalHeader(0, 3, naluTypeMTAP24),
		0x12, 0x34, /*                                                                */ // DONB
		0x00, 0x03, 0x55, 0x00, 0x00, 0x10, 0x67, 0x42, 0x1F, /*                      */ // size=3 NAL[3]
		0x00, 0x02, 0x56, 0x00, 0x00, 0x20, 0x68, 0xCE, /*                            */ // size=2 NAL[2]
	}
	got, err := d.Decode(100, payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := [][]byte{
		{0x67, 0x42, 0x1F},
		{0x68, 0xCE},
	}
	assertNALsEqual(t, got, want)
}

// ----- FU-A (type 28) ---------------------------------------------------------

func TestDecode_FUA_ThreePacketReassembly(t *testing.T) {
	d := &Depacketizer{}

	// Start: S=1, E=0, original NAL type=5 (IDR), NRI=3.
	p1 := []byte{
		nalHeader(0, 3, naluTypeFUA),
		fuHeader(1, 0, 0, 5),
		0xAA, 0xBB,
	}
	got, err := d.Decode(100, p1)
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("p1: got (%x, %v), want ErrIncomplete", got, err)
	}

	// Middle: S=0, E=0.
	p2 := []byte{
		nalHeader(0, 3, naluTypeFUA),
		fuHeader(0, 0, 0, 5),
		0xCC, 0xDD,
	}
	got, err = d.Decode(101, p2)
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("p2: got (%x, %v), want ErrIncomplete", got, err)
	}

	// End: S=0, E=1.
	p3 := []byte{
		nalHeader(0, 3, naluTypeFUA),
		fuHeader(0, 1, 0, 5),
		0xEE, 0xFF,
	}
	got, err = d.Decode(102, p3)
	if err != nil {
		t.Fatalf("p3 unexpected error: %v", err)
	}

	// Reassembled NAL: F=0, NRI=3, type=5 → header byte 0x65.
	want := [][]byte{{0x65, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}}
	assertNALsEqual(t, got, want)
}

func TestDecode_FUA_StartAndEndInOnePacket(t *testing.T) {
	// Forbidden by RFC 6184 §5.8 but observed in real encoders.
	d := &Depacketizer{}
	p := []byte{
		nalHeader(0, 3, naluTypeFUA),
		fuHeader(1, 1, 0, 5),
		0x11, 0x22,
	}
	got, err := d.Decode(100, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertNALsEqual(t, got, [][]byte{{0x65, 0x11, 0x22}})
}

func TestDecode_FUA_SeqGapAbandonsFragment(t *testing.T) {
	d := &Depacketizer{}

	p1 := []byte{
		nalHeader(0, 3, naluTypeFUA),
		fuHeader(1, 0, 0, 5),
		0xAA,
	}
	if _, err := d.Decode(100, p1); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("p1 expected ErrIncomplete, got %v", err)
	}

	// Skip seq 101 — a packet was lost.
	p3 := []byte{
		nalHeader(0, 3, naluTypeFUA),
		fuHeader(0, 1, 0, 5),
		0xCC,
	}
	if _, err := d.Decode(102, p3); err == nil {
		t.Fatal("expected error for FU sequence gap")
	}
	if d.fuActive {
		t.Error("FU state should be reset after a sequence gap")
	}
}

func TestDecode_FUA_ContinuationWithoutStartReturnsIncomplete(t *testing.T) {
	// At session entry we may join mid-FU and only see continuation
	// packets. We must not log them as drops; ErrIncomplete carries the
	// "wait for the next start" semantic.
	d := &Depacketizer{}
	p := []byte{
		nalHeader(0, 3, naluTypeFUA),
		fuHeader(0, 0, 0, 5),
		0xAA,
	}
	if _, err := d.Decode(100, p); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("got %v, want ErrIncomplete", err)
	}
}

// ----- FU-B (type 29) ---------------------------------------------------------

func TestDecode_FUB_ThreePacketReassembly_DONInStartOnly(t *testing.T) {
	d := &Depacketizer{}

	// Start: includes 2-byte DON between FU header and fragment data.
	p1 := []byte{
		nalHeader(0, 3, naluTypeFUB),
		fuHeader(1, 0, 0, 5),
		0xDE, 0xAD, // DON (must be skipped)
		0xAA, 0xBB,
	}
	if _, err := d.Decode(100, p1); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("p1 expected ErrIncomplete, got %v", err)
	}

	// Middle: still type 29, no DON.
	p2 := []byte{
		nalHeader(0, 3, naluTypeFUB),
		fuHeader(0, 0, 0, 5),
		0xCC, 0xDD,
	}
	if _, err := d.Decode(101, p2); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("p2 expected ErrIncomplete, got %v", err)
	}

	// End.
	p3 := []byte{
		nalHeader(0, 3, naluTypeFUB),
		fuHeader(0, 1, 0, 5),
		0xEE, 0xFF,
	}
	got, err := d.Decode(102, p3)
	if err != nil {
		t.Fatalf("p3 unexpected error: %v", err)
	}
	want := [][]byte{{0x65, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}}
	assertNALsEqual(t, got, want)
}

// ----- Cross-cutting ----------------------------------------------------------

func TestDecode_SingleNALAbandonsInProgressFragment(t *testing.T) {
	// If we start an FU and then receive a Single NAL packet for the same
	// stream, the FU should be abandoned (lost end fragment) and the
	// Single NAL passed through normally.
	d := &Depacketizer{}

	p1 := []byte{
		nalHeader(0, 3, naluTypeFUA),
		fuHeader(1, 0, 0, 5),
		0xAA,
	}
	if _, err := d.Decode(100, p1); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("p1 expected ErrIncomplete, got %v", err)
	}

	// Interrupting Single NAL.
	p2 := []byte{nalHeader(0, 2, 1), 0x99, 0x88}
	got, err := d.Decode(200, p2)
	if err != nil {
		t.Fatalf("interrupting single-NAL unexpected error: %v", err)
	}
	assertNALsEqual(t, got, [][]byte{p2})
	if d.fuActive {
		t.Error("FU state should be reset after a non-FU packet")
	}
}

func TestDecode_ReservedTypeIsError(t *testing.T) {
	d := &Depacketizer{}
	for _, typ := range []byte{0, 30, 31} {
		payload := []byte{typ, 0xAA}
		if _, err := d.Decode(100, payload); err == nil {
			t.Errorf("reserved NAL type %d: expected error, got nil", typ)
		}
	}
}

func TestDecode_EmptyPayloadIsError(t *testing.T) {
	d := &Depacketizer{}
	if _, err := d.Decode(100, nil); err == nil {
		t.Error("empty payload: expected error, got nil")
	}
}
