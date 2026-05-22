package mjpeg

import (
	"errors"
	"fmt"
	"strconv"

	"carvilon.local/stream/internal/profile"
)

// EncodeSpec describes a single MJPEG encode target by its raw
// parameters. Unlike [profile.Profile] (which is the public, named
// concept the admin manages), EncodeSpec is the internal encoder
// configuration — what ends up on the ffmpeg command line.
//
// Profiles map to EncodeSpecs via [DefaultSpecForUsage] today; later,
// the admin may set per-profile overrides, but the public Profile
// surface stays the same.
type EncodeSpec struct {
	Width   int // output pixels
	Height  int
	FPS     int // output frame rate; ffmpeg drops/dups to match
	Quality int // ffmpeg -q:v (1=best, 31=worst)
}

// Validate returns an error if any field is out of plausible range.
func (s EncodeSpec) Validate() error {
	if s.Width <= 0 || s.Width > 8192 {
		return fmt.Errorf("mjpeg: EncodeSpec.Width %d out of range (1..8192)", s.Width)
	}
	if s.Height <= 0 || s.Height > 8192 {
		return fmt.Errorf("mjpeg: EncodeSpec.Height %d out of range (1..8192)", s.Height)
	}
	if s.FPS <= 0 || s.FPS > 60 {
		return fmt.Errorf("mjpeg: EncodeSpec.FPS %d out of range (1..60)", s.FPS)
	}
	if s.Quality < 1 || s.Quality > 31 {
		return fmt.Errorf("mjpeg: EncodeSpec.Quality %d out of range (1..31)", s.Quality)
	}
	return nil
}

// OutputArgs returns the ffmpeg argument fragment that defines the OUTPUT
// side of the pipeline: filter chain + codec + format. The caller (the
// [Encoder]) prepends the static input arguments (`-f h264 -i pipe:0`
// etc.) and finishes with the output target (`pipe:1`).
//
// S6-06 — `-flags +bitexact`:
//
// libavcodec's MJPEG encoder by default inserts a COM marker (JPEG
// segment 0xFFFE) carrying the encoder's version string, e.g.
// `Lavc62.28.101`. The Espressif-P4 hardware JPEG decoder (ESP-IDF
// jpeg-driver) rejects this segment with
// `jpeg_parse_com_marker: COM marker data underflow` — every frame
// drops, the ESP screen stays black. Live-confirmed against
// .187:8555 in the S6-06 briefing.
//
// `-flags +bitexact` at the CODEC level (vs. `-fflags +bitexact` at
// the format/muxer level, which has no effect on this segment)
// suppresses the vendor string and therefore the entire COM marker.
// Encode quality is unchanged — only the metadata segment goes away,
// proven by the 18-byte file-size delta that exactly matches the
// COM-header overhead (2 marker + 2 length + 14 "Lavc62.28.101\0").
//
// Lives in OutputArgs() so all three MJPEG profiles (mjpeg_hq,
// mjpeg_bal, mjpeg_fast) inherit it through SpecFromProfile — one
// fix, three profiles.
func (s EncodeSpec) OutputArgs() []string {
	return []string{
		"-an", // no audio
		"-vf", fmt.Sprintf("scale=%d:%d", s.Width, s.Height),
		"-r", strconv.Itoa(s.FPS),
		"-c:v", "mjpeg",
		"-q:v", strconv.Itoa(s.Quality),
		// S6-06: suppress the libavcodec COM marker that the ESP-P4
		// HW decoder rejects. See doc comment above.
		"-flags", "+bitexact",
		"-f", "mjpeg",
	}
}

// ErrUnknownUsage is returned by [DefaultSpecForUsage] when the given
// usage has no default encode spec registered. Callers should fall
// back to a hard error or a sane default of their own.
var ErrUnknownUsage = errors.New("mjpeg: no default EncodeSpec for usage")

// ErrWrongCodec is returned by [SpecFromProfile] when the caller passes
// a profile whose Codec is not [profile.CodecMJPEG] — typically caught
// at the endpoint gate (/api/stream.mjpeg only serves MJPEG profiles).
var ErrWrongCodec = errors.New("mjpeg: profile codec is not mjpeg")

// SpecFromProfile reads the encode parameters straight off the profile.
// This is the S6-01 primary path: profiles persist their own Width /
// Height / FPS / EncodeQuality, so the encoder no longer needs a
// per-usage lookup table.
//
// Only profiles with Codec=[profile.CodecMJPEG] are accepted. The four
// fields are required by [profile.Profile.Validate] for that codec, so
// they are guaranteed to be set by the time we get here.
func SpecFromProfile(p profile.Profile) (EncodeSpec, error) {
	if p.Codec != profile.CodecMJPEG {
		return EncodeSpec{}, fmt.Errorf("%w: profile %q has codec %q", ErrWrongCodec, p.Name, p.Codec)
	}
	return EncodeSpec{
		Width:   p.Width,
		Height:  p.Height,
		FPS:     p.FPS,
		Quality: p.EncodeQuality,
	}, nil
}

// DefaultSpecForUsage maps a [profile.Usage] to the encoder defaults
// that the CARVILON ESP / browser pipeline has used in production.
// These are the values that ESP-Saison-2 and the go2rtc.yaml.example
// converged on:
//
//   - browser → 640x1024 @ 12 fps, q:v 5
//   - esp     → 800x1280 @  9 fps, q:v 6
//
// Deprecated (S6-01): prefer [SpecFromProfile]. Profiles now persist
// their own encode parameters; this lookup table only survives as a
// safety net for callers that haven't migrated yet and is exercised
// by the legacy tests.
func DefaultSpecForUsage(usage profile.Usage) (EncodeSpec, error) {
	switch usage {
	case profile.UsageBrowser:
		return EncodeSpec{Width: 640, Height: 1024, FPS: 12, Quality: 5}, nil
	case profile.UsageESP:
		return EncodeSpec{Width: 800, Height: 1280, FPS: 9, Quality: 6}, nil
	default:
		return EncodeSpec{}, fmt.Errorf("%w: %q", ErrUnknownUsage, usage)
	}
}
