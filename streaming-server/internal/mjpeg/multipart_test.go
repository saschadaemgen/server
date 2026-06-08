package mjpeg

import (
	"bytes"
	"mime"
	"net/http/httptest"
	"strings"
	"testing"
)

// Byte-exact assertion: the writer must produce EXACTLY the bytes
// go2rtc would, because the carvilon proxy forwards them verbatim.

func TestWriter_SingleFrame_ByteExact(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	jpeg := []byte{0xFF, 0xD8, 0xAA, 0xBB, 0xFF, 0xD9}
	n, err := w.Write(jpeg)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(jpeg) {
		t.Errorf("n = %d, want %d", n, len(jpeg))
	}

	want := []byte{}
	want = append(want, "--frame\r\nContent-Type: image/jpeg\r\nContent-Length: 6\r\n\r\n"...)
	want = append(want, jpeg...)
	want = append(want, "\r\n"...)

	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("output mismatch:\ngot:  %q\nwant: %q", buf.Bytes(), want)
	}
}

func TestWriter_TwoFrames_NoSeparator(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	a := []byte{0x01, 0x02}
	b := []byte{0x03, 0x04, 0x05}
	if _, err := w.Write(a); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}

	want := []byte{}
	want = append(want, "--frame\r\nContent-Type: image/jpeg\r\nContent-Length: 2\r\n\r\n"...)
	want = append(want, a...)
	want = append(want, "\r\n"...)
	want = append(want, "--frame\r\nContent-Type: image/jpeg\r\nContent-Length: 3\r\n\r\n"...)
	want = append(want, b...)
	want = append(want, "\r\n"...)

	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("two-frame output mismatch:\ngot:  %q\nwant: %q", buf.Bytes(), want)
	}
}

func TestWriter_ContentLengthDecimal(t *testing.T) {
	// Verify the length is written in plain decimal (no padding, no hex).
	var buf bytes.Buffer
	w := NewWriter(&buf)
	jpeg := make([]byte, 12345)
	if _, err := w.Write(jpeg); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("Content-Length: 12345\r\n")) {
		t.Errorf("Content-Length not decimal as expected; got: %s", buf.String()[:200])
	}
}

func TestWriter_FramePrefixIsConstant(t *testing.T) {
	// Locks the prefix in case anyone tweaks the headers under our feet.
	want := "--frame\r\nContent-Type: image/jpeg\r\nContent-Length: "
	if FramePrefix != want {
		t.Errorf("FramePrefix changed:\ngot:  %q\nwant: %q", FramePrefix, want)
	}
}

func TestSetResponseHeaders(t *testing.T) {
	rr := httptest.NewRecorder()
	SetResponseHeaders(rr)

	cases := []struct{ key, want string }{
		{"Content-Type", "multipart/x-mixed-replace; boundary=frame"},
		{"Cache-Control", "no-cache"},
		{"Connection", "close"},
		{"Pragma", "no-cache"},
	}
	for _, c := range cases {
		if got := rr.Header().Get(c.key); got != c.want {
			t.Errorf("%s: got %q, want %q", c.key, got, c.want)
		}
	}
}

func TestHeaderContentTypeHasSpaceAfterSemicolon(t *testing.T) {
	// go2rtc emits "multipart/x-mixed-replace; boundary=frame" with a
	// SPACE after the semicolon. Strict ESP firmware parsers have been
	// observed to depend on this. Lock it down.
	if HeaderContentType != "multipart/x-mixed-replace; boundary=frame" {
		t.Errorf("HeaderContentType changed: %q", HeaderContentType)
	}
}

// TestHeaderContentType_BoundaryParseable is the S6-05 ESP-facing
// sanity: feed our header through a standards-compliant parser (Go's
// mime.ParseMediaType) and assert the boundary parameter comes out
// NON-EMPTY and matches the body delimiter the Writer emits. This is
// the failure mode the ESP-Chat saw in their S4 — a buggy server
// shipped a Content-Type without a boundary= parameter, so the ESP
// parser came up with boundary="" and rejected the stream as
// "Header parsing failed".
//
// If this test breaks, the ESP fails the exact same way.
func TestHeaderContentType_BoundaryParseable(t *testing.T) {
	mediaType, params, err := mime.ParseMediaType(HeaderContentType)
	if err != nil {
		t.Fatalf("mime.ParseMediaType(%q): %v", HeaderContentType, err)
	}
	if mediaType != "multipart/x-mixed-replace" {
		t.Errorf("mediaType = %q, want multipart/x-mixed-replace", mediaType)
	}
	boundary := params["boundary"]
	if boundary == "" {
		t.Fatalf("boundary parameter is EMPTY (the bug the ESP-Chat reported in S4); params=%+v", params)
	}
	if boundary != "frame" {
		t.Errorf("boundary = %q, want %q", boundary, "frame")
	}

	// And the body delimiter MUST be "--" + boundary, per RFC 2046 §5.1.1.
	// If FramePrefix's leading literal drifts away from "--"+boundary, the
	// ESP would see headers say "boundary=frame" but the body use a
	// different delimiter — same blackness, harder to debug.
	wantDelim := "--" + boundary
	if !strings.HasPrefix(FramePrefix, wantDelim+"\r\n") {
		t.Errorf("FramePrefix %q does not start with %q+CRLF; body delimiter and header boundary diverged", FramePrefix, wantDelim)
	}
}

// TestWriter_FirstFrameByteLayout is the test that says: "if the ESP
// can't decode our stream, it isn't because of the framing". It pins
// down the first ~120 bytes of the wire stream as the ESP firmware
// parser would see them. The hex dump in the failure message is
// directly comparable against ESP-side captures.
func TestWriter_FirstFrameByteLayout(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	// Tiny JPEG-shaped payload — SOI + 4 garbage bytes + EOI. Real
	// JPEGs are bigger but the framing is identical.
	jpeg := []byte{0xFF, 0xD8, 0xDE, 0xAD, 0xBE, 0xEF, 0xFF, 0xD9}
	if _, err := w.Write(jpeg); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Expected wire bytes:
	//   "--frame\r\n"                       (boundary delimiter)
	//   "Content-Type: image/jpeg\r\n"
	//   "Content-Length: 8\r\n"
	//   "\r\n"                              (header/body separator)
	//   <8 JPEG bytes>
	//   "\r\n"                              (trailing CRLF before next boundary)
	expected := append([]byte(nil),
		'-', '-', 'f', 'r', 'a', 'm', 'e', '\r', '\n',
		'C', 'o', 'n', 't', 'e', 'n', 't', '-', 'T', 'y', 'p', 'e', ':', ' ',
		'i', 'm', 'a', 'g', 'e', '/', 'j', 'p', 'e', 'g', '\r', '\n',
		'C', 'o', 'n', 't', 'e', 'n', 't', '-', 'L', 'e', 'n', 'g', 't', 'h', ':', ' ',
		'8', '\r', '\n',
		'\r', '\n',
		0xFF, 0xD8, 0xDE, 0xAD, 0xBE, 0xEF, 0xFF, 0xD9,
		'\r', '\n',
	)
	if !bytes.Equal(buf.Bytes(), expected) {
		t.Errorf("wire bytes mismatch.\ngot:  % x\nwant: % x\nlen got=%d want=%d",
			buf.Bytes(), expected, buf.Len(), len(expected))
	}

	// Also assert that LF-only (the most common forgot-CRLF bug) is
	// NOT present anywhere outside the JPEG body. \r\n is mandatory
	// per RFC 2046; strict ESP parsers reject \n-only.
	header := buf.Bytes()[:len(buf.Bytes())-len(jpeg)-2] // strip trailing JPEG+CRLF
	for i, b := range header {
		if b == '\n' && (i == 0 || header[i-1] != '\r') {
			t.Errorf("bare LF (no leading CR) at header offset %d — RFC violation, ESP will likely reject", i)
		}
	}
}
