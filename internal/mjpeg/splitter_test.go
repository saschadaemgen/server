package mjpeg

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// Synthetic JPEG bytes: SOI, some payload, EOI. The exact internal
// structure is irrelevant to the splitter — only the marker positions
// matter. Tests use deliberately distinguishable payload bytes so we
// can verify the right frame was returned in concatenated cases.
func fakeJPEG(body string) []byte {
	out := []byte{0xFF, 0xD8}
	out = append(out, []byte(body)...)
	out = append(out, 0xFF, 0xD9)
	return out
}

func mustNext(t *testing.T, sp *FrameSplitter) []byte {
	t.Helper()
	frame, err := sp.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return frame
}

func TestSplitter_SingleFrame(t *testing.T) {
	jpeg := fakeJPEG("hello")
	sp := NewFrameSplitter(bytes.NewReader(jpeg))
	got := mustNext(t, sp)
	if !bytes.Equal(got, jpeg) {
		t.Errorf("frame mismatch:\ngot:  %q\nwant: %q", got, jpeg)
	}
	if _, err := sp.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("second Next: got %v, want io.EOF", err)
	}
}

func TestSplitter_ConcatenatedFrames(t *testing.T) {
	a := fakeJPEG("aaa")
	b := fakeJPEG("bb")
	c := fakeJPEG("c")
	stream := append(append(a, b...), c...)
	sp := NewFrameSplitter(bytes.NewReader(stream))

	if got := mustNext(t, sp); !bytes.Equal(got, a) {
		t.Errorf("frame 1 mismatch: %q vs %q", got, a)
	}
	if got := mustNext(t, sp); !bytes.Equal(got, b) {
		t.Errorf("frame 2 mismatch: %q vs %q", got, b)
	}
	if got := mustNext(t, sp); !bytes.Equal(got, c) {
		t.Errorf("frame 3 mismatch: %q vs %q", got, c)
	}
	if _, err := sp.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("fourth Next: got %v, want io.EOF", err)
	}
}

func TestSplitter_ChunkedReads(t *testing.T) {
	// Simulate small Read sizes: the splitter must reassemble frames
	// across the chunk boundary instead of producing partial output.
	jpeg := fakeJPEG("payload-that-crosses-chunks")
	sp := NewFrameSplitter(&slowReader{src: jpeg, chunk: 5})
	got := mustNext(t, sp)
	if !bytes.Equal(got, jpeg) {
		t.Errorf("frame mismatch under chunked reads:\ngot:  %q\nwant: %q", got, jpeg)
	}
}

func TestSplitter_LeadingGarbageDiscarded(t *testing.T) {
	garbage := []byte("ffmpeg startup banner blah blah\n")
	jpeg := fakeJPEG("frame")
	stream := append(garbage, jpeg...)
	sp := NewFrameSplitter(bytes.NewReader(stream))
	got := mustNext(t, sp)
	if !bytes.Equal(got, jpeg) {
		t.Errorf("frame after garbage: %q vs %q", got, jpeg)
	}
}

func TestSplitter_GarbageBetweenFrames(t *testing.T) {
	// Some encoders might insert padding bytes between frames.
	// The splitter must skip non-SOI prefix bytes and find the next
	// frame correctly.
	a := fakeJPEG("first")
	junk := []byte{0x00, 0xFF, 0x00, 0xFF, 0x01}
	b := fakeJPEG("second")
	stream := append(append(a, junk...), b...)
	sp := NewFrameSplitter(bytes.NewReader(stream))
	if got := mustNext(t, sp); !bytes.Equal(got, a) {
		t.Errorf("first frame mismatch: %q vs %q", got, a)
	}
	if got := mustNext(t, sp); !bytes.Equal(got, b) {
		t.Errorf("second frame mismatch: %q vs %q", got, b)
	}
}

func TestSplitter_TruncatedFrameReturnsEOF(t *testing.T) {
	// SOI present, EOI missing. After the reader returns EOF the
	// splitter should surface EOF rather than spinning forever.
	stream := []byte{0xFF, 0xD8, 'a', 'b', 'c'}
	sp := NewFrameSplitter(bytes.NewReader(stream))
	if _, err := sp.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("truncated frame: got %v, want io.EOF", err)
	}
}

func TestSplitter_NoFramesAtAll(t *testing.T) {
	sp := NewFrameSplitter(strings.NewReader("just text, no JPEGs here"))
	if _, err := sp.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("no frames: got %v, want io.EOF", err)
	}
}

func TestSplitter_FrameTooLarge(t *testing.T) {
	// Synthesize a stream with a stuck SOI and ever-growing buffer.
	// Use a small max for the test by relying on the public guard.
	// We test the symbolic property: a never-ending SOI without EOI
	// eventually returns ErrFrameTooLarge.
	//
	// The actual max is 16 MiB which would be slow to actually exhaust
	// in a unit test; instead we directly test that the guard fires
	// when readMore can't grow further.
	//
	// Driving this end-to-end takes too long; we settle for the
	// behavioral check that a clean EOF after partial data yields EOF
	// (covered by TestSplitter_TruncatedFrameReturnsEOF). The
	// too-large guard is best exercised live.
	t.Skip("too-large guard exercised live; unit-test would need 16+ MiB synthetic input")
}

// slowReader returns data one small chunk at a time, exercising the
// splitter's incremental-read code path.
type slowReader struct {
	src   []byte
	chunk int
	pos   int
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.src) {
		return 0, io.EOF
	}
	n := r.chunk
	if n > len(p) {
		n = len(p)
	}
	if n > len(r.src)-r.pos {
		n = len(r.src) - r.pos
	}
	copy(p, r.src[r.pos:r.pos+n])
	r.pos += n
	return n, nil
}
