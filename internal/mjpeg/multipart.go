// Package mjpeg implements the MJPEG output side of the CARVILON
// streaming kernel:
//
//   - [Writer] emits multipart/x-mixed-replace frames byte-for-byte
//     compatible with go2rtc's pkg/mjpeg/writer.go, so the carvilon
//     proxy and the ESP firmware see identical bytes whether the
//     backend is go2rtc or streaming-server.
//
//   - [FrameSplitter] scans the raw concatenated-JPEG output of
//     `ffmpeg -f mjpeg` into individual frames via SOI/EOI markers.
//
//   - [Profile] describes one configurable encode target (resolution,
//     frame rate, JPEG quality). The fields are strukturiert so an
//     admin UI can edit them later without code changes.
//
//   - [Encoder] wraps the ffmpeg subprocess that turns the upstream
//     H.264 stream into a profile-specific MJPEG output.
//
//   - [Hub] fans one Encoder's output to N HTTP clients, with the
//     same drop-statt-buffer discipline as the H.264 source bus
//     (internal/hub).
package mjpeg

import (
	"io"
	"net/http"
	"strconv"
)

// HeaderContentType is the value of the `Content-Type` HTTP response
// header for the MJPEG multipart stream. Matches go2rtc byte-for-byte
// (note the space after the semicolon — strict clients tolerate either
// form, but we mirror the established baseline).
const HeaderContentType = "multipart/x-mixed-replace; boundary=frame"

// FramePrefix is the per-frame multipart preamble, up to and including
// "Content-Length: ". A complete frame on the wire is:
//
//	FramePrefix + <decimal byte length> + "\r\n\r\n" + <jpeg bytes> + "\r\n"
//
// This exactly matches go2rtc/pkg/mjpeg/writer.go so the carvilon proxy
// can forward a streaming-server response without any byte rewriting.
const FramePrefix = "--frame\r\nContent-Type: image/jpeg\r\nContent-Length: "

// SetResponseHeaders writes the standard MJPEG response headers on w.
// Call before WriteHeader / the first Write. The headers match the set
// go2rtc emits.
func SetResponseHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", HeaderContentType)
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "close")
	h.Set("Pragma", "no-cache")
}

// Writer formats a sequence of JPEG byte slices as multipart MJPEG frames
// on an underlying io.Writer. One Write per JPEG frame.
//
// Use a single Writer per HTTP response. The instance maintains a small
// scratch buffer so per-frame allocations are limited to the (rare) case
// of a frame larger than the buffer's current capacity.
type Writer struct {
	w   io.Writer
	buf []byte
}

// NewWriter returns a Writer over the given destination. Call
// [SetResponseHeaders] before the first Write if w is an
// http.ResponseWriter; the Writer does not set headers itself.
func NewWriter(w io.Writer) *Writer {
	return &Writer{
		w:   w,
		buf: make([]byte, 0, len(FramePrefix)+16+4),
	}
}

// Write emits one multipart frame containing the given JPEG bytes:
//
//	--frame\r\n
//	Content-Type: image/jpeg\r\n
//	Content-Length: <n>\r\n
//	\r\n
//	<jpeg>\r\n
//
// Returns len(jpeg) and any error from the underlying writer. The caller
// is responsible for calling http.Flusher.Flush() after a successful
// Write so the bytes hit the wire immediately.
func (w *Writer) Write(jpeg []byte) (int, error) {
	w.buf = append(w.buf[:0], FramePrefix...)
	w.buf = strconv.AppendInt(w.buf, int64(len(jpeg)), 10)
	w.buf = append(w.buf, "\r\n\r\n"...)
	w.buf = append(w.buf, jpeg...)
	w.buf = append(w.buf, "\r\n"...)

	if _, err := w.w.Write(w.buf); err != nil {
		return 0, err
	}
	return len(jpeg), nil
}
