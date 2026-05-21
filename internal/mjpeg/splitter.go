package mjpeg

import (
	"bytes"
	"errors"
	"io"
)

// JPEG marker bytes used by [FrameSplitter].
const (
	soiByte0 = 0xFF
	soiByte1 = 0xD8 // Start Of Image
	eoiByte0 = 0xFF
	eoiByte1 = 0xD9 // End Of Image
)

// FrameSplitter scans an io.Reader carrying concatenated JPEG frames
// (as produced by `ffmpeg -f mjpeg`) and emits each complete frame.
//
// Frame boundaries are detected via the SOI marker (FF D8) at the start
// and the EOI marker (FF D9) at the end. Both markers are reserved by
// the JPEG specification and never appear inside the entropy-coded
// payload (encoders escape literal 0xFF bytes as 0xFF 0x00, and the
// in-band markers used during the scan — RST0..RST7, DHT, DQT, SOS, …
// — do not include 0xD8 or 0xD9). Scanning for these byte pairs
// therefore yields exact frame boundaries.
//
// Garbage bytes before the first SOI are silently discarded — this
// keeps us robust against any banner / startup-noise an ffmpeg variant
// might emit before the first encoded frame.
type FrameSplitter struct {
	r   io.Reader
	buf []byte
}

// Bounds on the internal scratch buffer. The initial size keeps small
// allocations cheap; the max guards against unbounded growth if the
// source produces non-JPEG garbage forever.
const (
	splitterInitialBuf = 64 * 1024
	splitterMaxBuf     = 16 * 1024 * 1024 // 16 MiB is way more than any sane JPEG frame
)

// NewFrameSplitter wraps r. Reads happen lazily on each [FrameSplitter.Next] call.
func NewFrameSplitter(r io.Reader) *FrameSplitter {
	return &FrameSplitter{r: r, buf: make([]byte, 0, splitterInitialBuf)}
}

// ErrFrameTooLarge is returned when the buffered, un-emitted bytes
// exceed [splitterMaxBuf] — a sign that the source is misbehaving.
var ErrFrameTooLarge = errors.New("mjpeg: buffered frame exceeds maximum size")

// Next returns the next complete JPEG frame, blocking until one is
// available or the underlying reader returns an error. On clean EOF
// after the last frame, Next returns (nil, io.EOF).
//
// The returned slice is freshly allocated; callers may retain it past
// further Next calls.
func (s *FrameSplitter) Next() ([]byte, error) {
	for {
		// Phase 1: locate SOI in whatever we have.
		soi := bytes.Index(s.buf, []byte{soiByte0, soiByte1})
		if soi < 0 {
			// Discard buffered garbage except the last byte, which
			// might be the leading 0xFF of an SOI that completes after
			// the next read.
			if len(s.buf) > 1 {
				s.buf = append(s.buf[:0], s.buf[len(s.buf)-1])
			}
			if err := s.readMore(); err != nil {
				return nil, err
			}
			continue
		}
		// Drop bytes before SOI.
		if soi > 0 {
			s.buf = s.buf[soi:]
		}

		// Phase 2: locate EOI after the SOI.
		// Start searching past the SOI (offset 2) to avoid matching
		// the very same SOI byte pair.
		searchFrom := 2
		eoi := bytes.Index(s.buf[searchFrom:], []byte{eoiByte0, eoiByte1})
		if eoi < 0 {
			if len(s.buf) > splitterMaxBuf {
				return nil, ErrFrameTooLarge
			}
			if err := s.readMore(); err != nil {
				return nil, err
			}
			continue
		}

		end := searchFrom + eoi + 2 // include the EOI bytes
		frame := make([]byte, end)
		copy(frame, s.buf[:end])
		s.buf = s.buf[end:]
		return frame, nil
	}
}

// readMore extends s.buf by one chunk worth of bytes from the
// underlying reader. Returns io.EOF only if the reader is exhausted
// AND nothing was read on this call.
func (s *FrameSplitter) readMore() error {
	// Grow capacity if we're near the end.
	if cap(s.buf)-len(s.buf) < 4096 {
		// Double capacity, up to the max.
		newCap := cap(s.buf) * 2
		if newCap > splitterMaxBuf {
			newCap = splitterMaxBuf
		}
		if newCap == cap(s.buf) {
			return ErrFrameTooLarge
		}
		grown := make([]byte, len(s.buf), newCap)
		copy(grown, s.buf)
		s.buf = grown
	}

	n, err := s.r.Read(s.buf[len(s.buf):cap(s.buf)])
	if n > 0 {
		s.buf = s.buf[:len(s.buf)+n]
		// Continue even on error if we read data; caller's next
		// iteration may yet find a complete frame in what we have.
		if err != nil && !errors.Is(err, io.EOF) {
			// Surface non-EOF errors; let next iteration discover them
			// once buffered bytes are processed.
			return nil
		}
		if errors.Is(err, io.EOF) {
			// EOF with data: the next call will try once more and
			// then surface EOF if no frame surfaces.
			return nil
		}
		return nil
	}
	return err
}
