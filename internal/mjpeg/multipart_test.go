package mjpeg

import (
	"bytes"
	"net/http/httptest"
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
