// Package h264esp transcodes the camera's H.264 (High profile) feed
// down to H.264 Constrained Baseline for the ESP32-P4 + tinyH264
// decode path, and serves the result as chunked HTTP at /stream/h264.
//
// Briefing-pinned wire shape (S6-02):
//
//   - Annex-B byte stream (start codes 00 00 00 01). NOT AVCC.
//   - NO AUDs (we do not pass h264_metadata=aud=insert to ffmpeg).
//   - SPS / PPS REPEATED in-band before every IDR/keyframe, not just
//     once globally. Enables clean late-join / reconnect on the ESP
//     side.
//   - Constrained Baseline profile (libx264 "baseline" maps here);
//     no B-frames, single reference, single slice per frame.
//   - One complete Access Unit per HTTP response chunk (the chunked
//     transfer-encoding IS the framing — no multipart, no custom AU
//     header). For keyframes: SPS + PPS + IDR coalesced into the
//     same chunk so the ESP can decode immediately.
//   - GoP = 1 second (keyint = fps). Runtime-tunable via PUT
//     /api/profiles/{name} as the ESP-chat brings back its VERIFY
//     numbers.
//
// Pipeline shape (one camera, N ESP clients):
//
//	Camera Hub  -->  ffmpeg subprocess (transcode high -> CBP)
//	                   |  stdout: Annex-B byte stream
//	                   v
//	             AUSplitter (NAL-aware, AU boundaries)
//	                   |
//	                   v
//	                Fan-out  -->  HTTP chunked, one AU per chunk
//
// Identical lifecycle posture to internal/mjpeg: one transcoder per
// profile, drop-statt-buffer per subscriber, transcoder dies if last
// viewer leaves (bedarfsgesteuert).
package h264esp

import (
	"errors"
	"fmt"
	"strconv"

	"carvilon.local/stream/internal/profile"
)

// EncodeSpec describes one Constrained Baseline encode target. Unlike
// [profile.Profile] (the admin-facing concept) this is the wire-shape
// of the ffmpeg command line.
//
// Quality is the libx264 CRF — 0 (lossless) .. 51 (worst); the
// practical low-latency sweet spot is 22..30. The S6 briefing starts
// at CRF 26 and lets the operator tune via PUT /api/profiles.
type EncodeSpec struct {
	Width   int // output pixels
	Height  int
	FPS     int // target frame rate
	Quality int // libx264 CRF (0..51; lower = better)
}

// Validate returns an error if any field is out of plausible range.
func (s EncodeSpec) Validate() error {
	if s.Width <= 0 || s.Width > 8192 {
		return fmt.Errorf("h264esp: EncodeSpec.Width %d out of range (1..8192)", s.Width)
	}
	if s.Height <= 0 || s.Height > 8192 {
		return fmt.Errorf("h264esp: EncodeSpec.Height %d out of range (1..8192)", s.Height)
	}
	if s.FPS <= 0 || s.FPS > 60 {
		return fmt.Errorf("h264esp: EncodeSpec.FPS %d out of range (1..60)", s.FPS)
	}
	if s.Quality < 0 || s.Quality > 51 {
		return fmt.Errorf("h264esp: EncodeSpec.Quality (CRF) %d out of range (0..51)", s.Quality)
	}
	return nil
}

// OutputArgs returns the ffmpeg argument fragment for the output side
// of the pipeline (filter chain + codec + format). The caller (the
// [Encoder]) prepends the static input args and appends `pipe:1`.
//
// Why these flags, briefing-by-briefing:
//
//   - `-c:v libx264 -profile:v baseline`: Constrained Baseline. libx264
//     "baseline" alone implies CBP (no FMO/ASO/redundant slices,
//     which would be plain "baseline" and almost nothing supports).
//   - `-level 3.0`: 720p @ 30fps fits inside; safe enough for the ESP
//     decoder. If a higher target ever appears, this becomes
//     spec-driven.
//   - `-preset ultrafast -tune zerolatency`: minimum encoder latency
//     and CPU cost. ESP is decoding live; we don't have a budget for
//     "veryslow".
//   - `-x264-params sliced-threads=0:slices=1`: belt-and-braces single-
//     slice enforcement. `-tune zerolatency` ALONE turns on libx264's
//     `sliced-threads=1` (slice-level parallelism: N slices per frame,
//     one per worker thread). Our splitter's contract is "one VCL NAL
//     == one Access Unit" — multi-slice frames would inflate the
//     emitted-AU count 4-10x and make /stream/stats avg_fps useless
//     for the codec comparison the whole experiment hangs on. This
//     S6-04 fix forces back to the briefing's "ein Slice pro Frame"
//     default. Frame-level threading still works via -threads.
//   - `-bf 0`: no B-frames. CBP forbids them anyway, but be explicit.
//   - `-refs 1`: single reference frame. Lowest decoder memory; the
//     ESP cares.
//   - `-g <fps>` + `-keyint_min <fps>`: GoP = 1 second exactly. The
//     briefing's S6-02 starting value; runtime-tunable.
//   - `-sc_threshold 0`: never insert a scene-change keyframe. We
//     want predictable GoP=fps, not whatever a scene-detector thinks.
//   - `-bsf:v dump_extra=freq=keyframe`: REPEAT SPS+PPS in-band
//     before every IDR. Without this, ffmpeg writes the headers once
//     at the very start of the stream and a late-joining ESP would
//     never see them. The =freq=keyframe knob is the one that gates
//     on IDR (not every NAL).
//   - `-an`: no audio.
//   - `-vf scale=W:H`, `-r FPS`: spec-driven output shape.
//   - `-crf <Q>`: rate control. CRF gives constant-quality output
//     better suited to a single-camera experiment than CBR.
//   - `-f h264`: write the raw Annex-B byte stream to pipe:1. NO
//     container, NO AUDs (which would be `-bsf:v h264_metadata=aud=
//     insert` — we leave it out).
func (s EncodeSpec) OutputArgs() []string {
	gop := strconv.Itoa(s.FPS) // GoP = 1 second
	return []string{
		"-an",
		"-vf", fmt.Sprintf("scale=%d:%d", s.Width, s.Height),
		"-r", strconv.Itoa(s.FPS),
		"-c:v", "libx264",
		"-profile:v", "baseline",
		"-level", "3.0",
		"-preset", "ultrafast",
		"-tune", "zerolatency",
		// S6-04: undo the sliced-threads=1 that -tune zerolatency
		// implicitly turns on, and pin slices=1. See the package doc
		// comment above for why this matters.
		"-x264-params", "sliced-threads=0:slices=1",
		"-bf", "0",
		"-refs", "1",
		"-g", gop,
		"-keyint_min", gop,
		"-sc_threshold", "0",
		"-crf", strconv.Itoa(s.Quality),
		// The two BSFs below are the "wire-shape" half: repeat SPS+PPS
		// before every IDR, write Annex-B without AUDs.
		"-bsf:v", "dump_extra=freq=keyframe",
		"-f", "h264",
	}
}

// ErrWrongCodec is returned by [SpecFromProfile] when the caller
// passes a profile whose Codec isn't [profile.CodecH264CBP].
var ErrWrongCodec = errors.New("h264esp: profile codec is not h264_cbp")

// SpecFromProfile reads the encode parameters off the profile. Only
// profiles with Codec == [profile.CodecH264CBP] are accepted; the
// four fields (Width/Height/FPS/EncodeQuality) are required by
// [profile.Profile.Validate] for that codec.
func SpecFromProfile(p profile.Profile) (EncodeSpec, error) {
	if p.Codec != profile.CodecH264CBP {
		return EncodeSpec{}, fmt.Errorf("%w: profile %q has codec %q", ErrWrongCodec, p.Name, p.Codec)
	}
	return EncodeSpec{
		Width:   p.Width,
		Height:  p.Height,
		FPS:     p.FPS,
		Quality: p.EncodeQuality,
	}, nil
}
