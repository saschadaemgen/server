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
func (s EncodeSpec) OutputArgs() []string {
	return []string{
		"-an", // no audio
		"-vf", fmt.Sprintf("scale=%d:%d", s.Width, s.Height),
		"-r", strconv.Itoa(s.FPS),
		"-c:v", "mjpeg",
		"-q:v", strconv.Itoa(s.Quality),
		"-f", "mjpeg",
	}
}

// ErrUnknownUsage is returned by [DefaultSpecForUsage] when the given
// usage has no default encode spec registered. Callers should fall
// back to a hard error or a sane default of their own.
var ErrUnknownUsage = errors.New("mjpeg: no default EncodeSpec for usage")

// DefaultSpecForUsage maps a [profile.Usage] to the encoder defaults
// that the CARVILON ESP / browser pipeline has used in production.
// These are the values that ESP-Saison-2 and the go2rtc.yaml.example
// converged on:
//
//   - browser → 640x1024 @ 12 fps, q:v 5
//   - esp     → 800x1280 @  9 fps, q:v 6
//
// "Hinter der Naht" (ADR-STREAM-01): the admin will eventually be able
// to override these per profile, but the public Profile struct shape
// is independent of the encoder details — usage-based defaults remain
// the fallback. Any future android/iOS usages get added here.
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
