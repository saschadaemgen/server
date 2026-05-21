package mjpeg

import (
	"errors"
	"fmt"
	"strconv"
)

// Profile describes one MJPEG encode target by its parameters, NOT by a
// pre-baked ffmpeg argument string. Keeping the fields strukturiert lets
// the upcoming admin UI (ADR-STREAM-01) edit them without code changes,
// and lets us add arbitrarily many use cases (esp / browser / android /
// future) without growing a switch statement.
type Profile struct {
	// Name is the value clients pass in ?src=. Convention so far:
	// "intercom_esp", "intercom_browser". The admin UI will let
	// operators add more (e.g. "intercom_android") without code.
	Name string

	// Width and Height are the output pixel dimensions. The ffmpeg
	// scale filter is fed exactly these.
	Width  int
	Height int

	// FPS is the output frame rate. ffmpeg's -r flag drops/duplicates
	// to hit this rate regardless of the source rate.
	FPS int

	// Quality is the JPEG quality on ffmpeg's mjpeg encoder (-q:v):
	// 1 = best, 31 = worst. 5–6 is the practical sweet spot from the
	// go2rtc ESP-Saison-2 setup.
	Quality int
}

// Validate returns an error if any field is out of plausible range.
// Cheap defence against admin-UI typos before they reach ffmpeg.
func (p Profile) Validate() error {
	if p.Name == "" {
		return errors.New("mjpeg: profile Name is required")
	}
	if p.Width <= 0 || p.Width > 8192 {
		return fmt.Errorf("mjpeg: profile %q: Width %d out of range (1..8192)", p.Name, p.Width)
	}
	if p.Height <= 0 || p.Height > 8192 {
		return fmt.Errorf("mjpeg: profile %q: Height %d out of range (1..8192)", p.Name, p.Height)
	}
	if p.FPS <= 0 || p.FPS > 60 {
		return fmt.Errorf("mjpeg: profile %q: FPS %d out of range (1..60)", p.Name, p.FPS)
	}
	if p.Quality < 1 || p.Quality > 31 {
		return fmt.Errorf("mjpeg: profile %q: Quality %d out of range (1..31)", p.Name, p.Quality)
	}
	return nil
}

// OutputArgs returns the ffmpeg argument fragment that defines the OUTPUT
// side of the pipeline: filter chain + codec + format. The caller (the
// [Encoder]) prepends the static input arguments (`-f h264 -i pipe:0`
// etc.) and finishes with the output target (`pipe:1`).
//
// Keeping output args separate keeps the responsibility split clean:
// the profile describes WHAT to produce, the encoder decides HOW the
// pipeline is plumbed.
func (p Profile) OutputArgs() []string {
	return []string{
		"-an", // no audio
		"-vf", fmt.Sprintf("scale=%d:%d", p.Width, p.Height),
		"-r", strconv.Itoa(p.FPS),
		"-c:v", "mjpeg",
		"-q:v", strconv.Itoa(p.Quality),
		"-f", "mjpeg",
	}
}

// DefaultProfiles returns the starting set that mirrors the working
// go2rtc setup from the ESP project — drop-in compatible.
//
// Values per BRIEFING-STREAM-S3-02 and go2rtc.yaml.example:
//   - intercom_esp:     800x1280 @ 9 fps, quality 6
//   - intercom_browser: 640x1024 @ 12 fps, quality 5
//
// These are STARTPUNKT values; the admin UI will eventually edit them
// live. They are NOT hardcoded into the encoder — the encoder reads
// them off this struct.
func DefaultProfiles() []Profile {
	return []Profile{
		{Name: "intercom_esp", Width: 800, Height: 1280, FPS: 9, Quality: 6},
		{Name: "intercom_browser", Width: 640, Height: 1024, FPS: 12, Quality: 5},
	}
}
