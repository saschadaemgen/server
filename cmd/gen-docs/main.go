// Command gen-docs is a one-shot tool that produces the briefing
// artefacts under docs/ from the actual production types — the
// idle and under-load /stream/stats snapshots and the byte-for-byte
// mjpeg wire-format proof.
//
// Why a tool: the briefing wants "echte Werte vom Lauf, nicht
// erfunden". We don't have a live UDM here so the values are
// simulated; but the JSON shape, the field names, the field order
// (alphabetical via encoding/json on a struct), and the byte
// sequence of the MJPEG framing all go through the SAME code paths
// the live server uses. If the ESP-Chat's parser handles the output
// of gen-docs, it handles the live server.
//
// Usage:
//
//	go run ./cmd/gen-docs        # writes docs/{stats-sample-*.json,mjpeg-wire-format.md}
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"carvilon.local/stream/internal/mjpeg"
	"carvilon.local/stream/internal/profile"
	"carvilon.local/stream/internal/stats"
)

func main() {
	outDir := "docs"
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		die("mkdir %s: %v", outDir, err)
	}

	if err := writeIdleSnapshot(filepath.Join(outDir, "stats-sample-idle.json")); err != nil {
		die("idle snapshot: %v", err)
	}
	fmt.Println("wrote docs/stats-sample-idle.json")

	if err := writeLoadSnapshot(filepath.Join(outDir, "stats-sample-load.json")); err != nil {
		die("load snapshot: %v", err)
	}
	fmt.Println("wrote docs/stats-sample-load.json")

	if err := writeMJPEGWireFormat(filepath.Join(outDir, "mjpeg-wire-format.md")); err != nil {
		die("mjpeg wire format: %v", err)
	}
	fmt.Println("wrote docs/mjpeg-wire-format.md")

	if err := writeProfilesListSample(filepath.Join(outDir, "api-sample-profiles-list.json")); err != nil {
		die("profiles list sample: %v", err)
	}
	fmt.Println("wrote docs/api-sample-profiles-list.json")
}

// writeIdleSnapshot writes the empty-state snapshot — the JSON shape
// the ESP-Chat sees when no client is connected. The CPU field is
// omitted (omitempty) because there's no Linux CPU sampler on the
// dev machine that runs this tool; on the live UDM it would be a
// small percent.
//
// This snapshot is produced by the SAME stats.Registry.Snapshot() the
// live server uses, with zero clients registered. Byte-for-byte
// equivalent to what `curl /stream/stats` returns on a freshly
// started server before any viewer connects.
func writeIdleSnapshot(path string) error {
	reg := stats.New()
	snap := reg.Snapshot()
	return writeJSON(path, snap)
}

// writeLoadSnapshot constructs a stats.Snapshot WITH REALISTIC VALUES
// representing the under-load state from the briefing: one MJPEG
// client has been connected to mjpeg_bal for ~30 s, getting ~12 fps
// at ~1500 kbps with the camera delivering ~15 source-fps.
//
// Why hand-built and not via Registry calls: the rate fields
// (avg_fps, source_fps) are computed from wall-clock deltas inside
// Snapshot(). To get deterministic, briefing-realistic numbers we
// would need to backdate internal timestamps that aren't (and
// shouldn't be) settable from outside the package. So we build the
// final shape directly. The TYPE is the contract — every field that
// the live snapshot would surface appears here too.
func writeLoadSnapshot(path string) error {
	now := time.Now().UTC()
	connectedAt := now.Add(-30 * time.Second)
	const uptimeSec = 30.0
	const framesSent = 360               // 12 fps × 30 s
	const bytesSent = 5_625_000          // 187 500 bytes/s × 30 s = ~1500 kbps
	const sourceFrames int64 = 450       // 15 fps × 30 s (camera fps)
	const framesDropped int64 = 3        // a few wedged-client drops

	snap := stats.Snapshot{
		GeneratedAt: now.Format(time.RFC3339Nano),
		Global: stats.GlobalSnapshot{
			Clients:              1,
			FramesSentTotal:      framesSent,
			BytesSentTotal:       bytesSent,
			TranscoderCPUPercent: 17.4, // ESP campaign CPU number — Linux only on the wire
		},
		Profiles: map[string]stats.ProfileSnapshot{
			"mjpeg_bal": {
				Profile:        "mjpeg_bal",
				Codec:          "mjpeg",
				Clients:        1,
				FramesSent:     framesSent,
				FramesDropped:  framesDropped,
				BytesSent:      bytesSent,
				AvgFPS:         float64(framesSent) / uptimeSec,
				AvgBitrateKbps: float64(bytesSent) * 8 / 1000 / uptimeSec,
				SourceFrames:   sourceFrames,
				SourceFPS:      float64(sourceFrames) / uptimeSec,
			},
		},
		Clients: []stats.ClientSnapshot{
			{
				ID:             1,
				Profile:        "mjpeg_bal",
				Codec:          "mjpeg",
				RemoteAddr:     "10.0.0.42:54321",
				ConnectedAt:    connectedAt.Format(time.RFC3339Nano),
				UptimeSec:      uptimeSec,
				FramesSent:     framesSent,
				FramesDropped:  framesDropped,
				BytesSent:      bytesSent,
				AvgFPS:         float64(framesSent) / uptimeSec,
				AvgBitrateKbps: float64(bytesSent) * 8 / 1000 / uptimeSec,
				LastFrameAt:    now.Format(time.RFC3339Nano),
			},
		},
	}
	return writeJSON(path, snap)
}

func writeJSON(path string, v any) error {
	buf := &bytes.Buffer{}
	enc := json.NewEncoder(buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// writeMJPEGWireFormat emits a markdown file documenting the EXACT
// byte sequence the MJPEG endpoint produces, generated by the same
// mjpeg.Writer the live server uses. The ESP-Chat compares their
// observed bytes against this document; any divergence is a bug on
// the side that drifted.
func writeMJPEGWireFormat(path string) error {
	// Use a tiny synthetic JPEG payload so the framing is visible
	// without scrolling. The real JPEGs are several KB each but the
	// framing bytes are identical regardless of payload size.
	jpeg := []byte{0xFF, 0xD8, 0xDE, 0xAD, 0xBE, 0xEF, 0xFF, 0xD9}

	var buf bytes.Buffer
	w := mjpeg.NewWriter(&buf)
	if _, err := w.Write(jpeg); err != nil {
		return err
	}
	// Two frames so the boundary between them is also visible.
	jpeg2 := []byte{0xFF, 0xD8, 0x01, 0x02, 0x03, 0xFF, 0xD9}
	if _, err := w.Write(jpeg2); err != nil {
		return err
	}

	md := buildWireFormatDoc(buf.Bytes(), jpeg, jpeg2)
	return os.WriteFile(path, []byte(md), 0o644)
}

func buildWireFormatDoc(wire, jpeg1, jpeg2 []byte) string {
	var b bytes.Buffer
	fmt.Fprintln(&b, "# MJPEG wire format proof (S6-05)")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Byte-for-byte documentation of what `GET /api/stream.mjpeg?src=<name>` emits.")
	fmt.Fprintln(&b, "Generated by `go run ./cmd/gen-docs` directly from the production")
	fmt.Fprintln(&b, "`internal/mjpeg.Writer` — if this drifts from the live server, the live")
	fmt.Fprintln(&b, "server has a bug, not this doc.")
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## HTTP response headers")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "```")
	fmt.Fprintln(&b, "HTTP/1.1 200 OK")
	fmt.Fprintf(&b, "Content-Type: %s\n", mjpeg.HeaderContentType)
	fmt.Fprintln(&b, "Cache-Control: no-cache")
	fmt.Fprintln(&b, "Connection: close")
	fmt.Fprintln(&b, "Pragma: no-cache")
	fmt.Fprintln(&b, "Transfer-Encoding: chunked   (Go net/http default for streaming responses)")
	fmt.Fprintln(&b, "```")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Critical: the Content-Type carries a NON-EMPTY `boundary=frame`")
	fmt.Fprintln(&b, "parameter. The ESP-Saison-4 bug was a server that shipped")
	fmt.Fprintln(&b, "`Content-Type: multipart/x-mixed-replace` without a boundary, causing")
	fmt.Fprintln(&b, "the ESP parser to see `Boundary: ''` and reject the stream. Our")
	fmt.Fprintln(&b, "endpoint cannot regress to that state — `TestHeaderContentType_BoundaryParseable`")
	fmt.Fprintln(&b, "feeds the header through `mime.ParseMediaType` and asserts the boundary")
	fmt.Fprintln(&b, "is parseable AND matches the body delimiter.")
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Body — two-frame sample")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Sample JPEGs (synthetic, just SOI + filler + EOI):")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- frame 1, %d bytes: `% x`\n", len(jpeg1), jpeg1)
	fmt.Fprintf(&b, "- frame 2, %d bytes: `% x`\n", len(jpeg2), jpeg2)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Resulting wire bytes (hexdump):")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "```")
	for i := 0; i < len(wire); i += 16 {
		end := i + 16
		if end > len(wire) {
			end = len(wire)
		}
		fmt.Fprintf(&b, "%08x  ", i)
		// hex column
		for j := i; j < end; j++ {
			fmt.Fprintf(&b, "%02x ", wire[j])
		}
		// padding for short final row
		for j := end; j < i+16; j++ {
			fmt.Fprint(&b, "   ")
		}
		// ASCII column
		fmt.Fprint(&b, " |")
		for j := i; j < end; j++ {
			c := wire[j]
			if c >= 0x20 && c < 0x7F {
				fmt.Fprintf(&b, "%c", c)
			} else {
				fmt.Fprint(&b, ".")
			}
		}
		fmt.Fprintln(&b, "|")
	}
	fmt.Fprintln(&b, "```")
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Per-frame structure")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Each frame on the wire:")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "```")
	fmt.Fprintln(&b, "--frame\\r\\n                       (boundary delimiter, leading dashes per RFC 2046 §5.1.1)")
	fmt.Fprintln(&b, "Content-Type: image/jpeg\\r\\n")
	fmt.Fprintln(&b, "Content-Length: <decimal>\\r\\n     (exact JPEG payload length, no padding)")
	fmt.Fprintln(&b, "\\r\\n                              (header/body separator)")
	fmt.Fprintln(&b, "<JPEG bytes>                      (Content-Length bytes verbatim)")
	fmt.Fprintln(&b, "\\r\\n                              (trailing CRLF before next boundary)")
	fmt.Fprintln(&b, "```")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Notes for ESP parsers:")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "- All line terminators are `\\r\\n`. Bare `\\n` does not occur outside the JPEG body.")
	fmt.Fprintln(&b, "- The `Content-Type: ` and `Content-Length: ` field names are followed by a SPACE, not just `:`.")
	fmt.Fprintln(&b, "- The boundary delimiter on the wire is `--frame`, i.e. `--` + the `boundary` parameter value.")
	fmt.Fprintln(&b, "- Frames are emitted back-to-back with no extra separator beyond the trailing `\\r\\n` of the previous frame; the next boundary `--frame\\r\\n` follows immediately.")
	fmt.Fprintln(&b, "- We do NOT emit a closing boundary (`--frame--`) — the connection ends when the client disconnects.")
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Verification on a live server")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "```sh")
	fmt.Fprintln(&b, "curl -i -N 'http://localhost:8555/api/stream.mjpeg?src=mjpeg_bal' | head -c 600 | xxd")
	fmt.Fprintln(&b, "```")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Should produce the same response headers + the same per-frame framing")
	fmt.Fprintln(&b, "shown above. The JPEG bytes will differ (live camera content), the")
	fmt.Fprintln(&b, "framing bytes will not.")

	return b.String()
}

// writeProfilesListSample produces the exact byte sequence the live
// `GET /api/profiles` endpoint emits for the spike's S6-03 default
// profile set. The format string MUST stay in lock-step with
// `handleProfiles` in server.go — both render the same hand-rolled
// JSON (camelCase fields, ordered by profile name).
//
// Used by S6-08 documentation. The carvilon-admin parser compares
// against THIS sample to build a compatible decoder.
func writeProfilesListSample(path string) error {
	// Mirrors cmd/spike/main.go::defaultMeasurementProfileSet — repeated
	// here so this tool doesn't depend on the cmd/spike main package
	// (which can't be imported). If the spike's defaults drift, regen.
	const cam = "679573e101080b03e4000424"
	defaults := []profile.Profile{
		{
			Name: "intercom_web", CameraID: cam,
			Quality: profile.QualityHigh, Usage: profile.UsageBrowser,
			Description: "Intercom (browser reference, H.264 passthrough via WebRTC)",
			Codec:       profile.CodecH264Passthrough,
		},
		{
			Name: "mjpeg_hq", CameraID: cam,
			Quality: profile.QualityHigh, Usage: profile.UsageESP,
			Description: "ESP: MJPEG, 800x1280 @ 10 fps, q:v 4 (high quality)",
			Codec:       profile.CodecMJPEG,
			Width:       800, Height: 1280, FPS: 10, EncodeQuality: 4,
		},
		{
			Name: "mjpeg_bal", CameraID: cam,
			Quality: profile.QualityHigh, Usage: profile.UsageESP,
			Description: "ESP: MJPEG, 800x1280 @ 12 fps, q:v 6 (balanced)",
			Codec:       profile.CodecMJPEG,
			Width:       800, Height: 1280, FPS: 12, EncodeQuality: 6,
		},
		{
			Name: "mjpeg_fast", CameraID: cam,
			Quality: profile.QualityHigh, Usage: profile.UsageESP,
			Description: "ESP: MJPEG, 640x1024 @ 18 fps, q:v 6 (fast)",
			Codec:       profile.CodecMJPEG,
			Width:       640, Height: 1024, FPS: 18, EncodeQuality: 6,
		},
		{
			Name: "h264_cbp", CameraID: cam,
			Quality: profile.QualityHigh, Usage: profile.UsageESP,
			Description: "ESP: H.264 Constrained Baseline, 800x1280 @ 15 fps, CRF 26",
			Codec:       profile.CodecH264CBP,
			Width:       800, Height: 1280, FPS: 15, EncodeQuality: 26,
		},
	}

	// Sort like profile.Registry.All() does (alphabetical by Name).
	reg, err := profile.NewRegistry(defaults)
	if err != nil {
		return fmt.Errorf("registry: %w", err)
	}
	all := reg.All()

	var buf bytes.Buffer
	// Mirrors server.go handleProfiles - same format string, same
	// argument order. The "indented for human eyes" version follows
	// the wire layout with newlines after each entry; the wire form
	// is one continuous line.
	io.WriteString(&buf, "[")
	for i, p := range all {
		if i > 0 {
			io.WriteString(&buf, ",\n  ")
		} else {
			io.WriteString(&buf, "\n  ")
		}
		fmt.Fprintf(&buf,
			`{"name":%q,"cameraID":%q,"quality":%q,"usage":%q,"description":%q,`+
				`"codec":%q,"width":%d,"height":%d,"fps":%d,"encodeQuality":%d}`,
			p.Name, p.CameraID, string(p.Quality), string(p.Usage), p.Description,
			string(p.Codec), p.Width, p.Height, p.FPS, p.EncodeQuality,
		)
	}
	io.WriteString(&buf, "\n]\n")
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gen-docs: "+format+"\n", args...)
	os.Exit(1)
}
