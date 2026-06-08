package mjpeg

import (
	"strings"
	"testing"
)

// Encoder mostly drives an external ffmpeg subprocess, which is hard to
// unit-test in isolation. We focus tests on:
//
//   - argument construction (buildFFmpegArgs)
//   - CheckFFmpeg error path on a missing binary
//   - constructor validation + default handling
//
// The subprocess plumbing is exercised by the live test.

func TestBuildFFmpegArgs_LayoutMatchesContract(t *testing.T) {
	s := EncodeSpec{Width: 800, Height: 1280, FPS: 9, Quality: 6}
	args := buildFFmpegArgs(s)

	// Input side: -f h264 -i pipe:0 must appear in that order, ahead
	// of any output options.
	iH264 := indexOf(args, "h264")
	iPipe0 := indexOf(args, "pipe:0")
	if iH264 < 0 || iPipe0 < 0 || iH264 > iPipe0 {
		t.Errorf("input args malformed: %v", args)
	}
	if args[iH264-1] != "-f" {
		t.Errorf("expected -f before h264; got %v", args[max(0, iH264-2):iH264+1])
	}
	if args[iPipe0-1] != "-i" {
		t.Errorf("expected -i before pipe:0; got %v", args[max(0, iPipe0-2):iPipe0+1])
	}

	// Output side: -c:v mjpeg -f mjpeg pipe:1 must end the command.
	if args[len(args)-1] != "pipe:1" {
		t.Errorf("last arg = %q, want pipe:1 (full: %v)", args[len(args)-1], args)
	}

	// Spec values present. S6-13: fps + scale live in a single -vf
	// filter chain (`fps=N,scale=W:H`); no bare -r at output.
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-vf fps=9,scale=800:1280", "-q:v 6", "-c:v mjpeg", "-f mjpeg",
		"-hide_banner", "-loglevel error", "-nostats", "+nobuffer",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected %q in ffmpeg args; got: %v", want, args)
		}
	}
}

func TestBuildFFmpegArgs_SpecValuesFlowThrough(t *testing.T) {
	s := EncodeSpec{Width: 1280, Height: 720, FPS: 25, Quality: 3}
	args := buildFFmpegArgs(s)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "fps=25,scale=1280:720") {
		t.Errorf("expected fps=25,scale=1280:720 in -vf chain; got %v", args)
	}
	if !strings.Contains(joined, "-q:v 3") {
		t.Errorf("expected -q:v 3; got %v", args)
	}
}

func TestNewEncoder_RejectsInvalidSpec(t *testing.T) {
	_, err := NewEncoder(EncoderOptions{
		Label: "x",
		Spec:  EncodeSpec{Width: 0, Height: 1280, FPS: 9, Quality: 6},
	})
	if err == nil {
		t.Fatal("expected error for zero Width")
	}
}

func TestNewEncoder_AppliesDefaults(t *testing.T) {
	enc, err := NewEncoder(EncoderOptions{
		Label: "x",
		Spec:  EncodeSpec{Width: 100, Height: 100, FPS: 10, Quality: 5},
	})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if enc.ffmpegPath != "ffmpeg" {
		t.Errorf("default ffmpegPath = %q, want %q", enc.ffmpegPath, "ffmpeg")
	}
	if cap(enc.inputCh) != defaultInputBuf {
		t.Errorf("default input buf = %d, want %d", cap(enc.inputCh), defaultInputBuf)
	}
	if cap(enc.outputCh) != defaultOutputBuf {
		t.Errorf("default output buf = %d, want %d", cap(enc.outputCh), defaultOutputBuf)
	}
}

// TestEncoderDefaults_LowLatencySized locks the post-S6-07 buffer
// sizes so a "the channels look small, surely we can bump them"
// drive-by doesn't quietly bring back the 2 s lag.
func TestEncoderDefaults_LowLatencySized(t *testing.T) {
	const maxLatencyTolerantBuf = 4
	if defaultInputBuf > maxLatencyTolerantBuf {
		t.Errorf("defaultInputBuf = %d, want <= %d (S6-07 latency floor)", defaultInputBuf, maxLatencyTolerantBuf)
	}
	if defaultOutputBuf > maxLatencyTolerantBuf {
		t.Errorf("defaultOutputBuf = %d, want <= %d (S6-07 latency floor)", defaultOutputBuf, maxLatencyTolerantBuf)
	}
}

func TestNewEncoder_DefaultLabel(t *testing.T) {
	enc, err := NewEncoder(EncoderOptions{
		Spec: EncodeSpec{Width: 100, Height: 100, FPS: 10, Quality: 5},
	})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if enc.Label() == "" {
		t.Error("empty label should be replaced with a default")
	}
}

// TestBuildFFmpegArgs_NoCodecLowDelay is the S2-16 throughput canary
// (it replaces the S6-07 latency canary that asserted the opposite).
//
// S6-07 once added `-flags +low_delay` (codec-level) to trim a couple
// of frames of decode latency. S2-16 measured on the RPi4 that this
// flag ALSO disables ffmpeg's multi-core frame threading, forcing
// single-threaded H.264 decode. With the 1200x1600 High Profile source
// that meant decode could no longer keep up with the source rate
// (~1.3x vs ~2x realtime without it); the encoder input backed up, and
// at GOP ~105 each lost P-frame smeared the image for up to ~5 s.
//
// This test fails LOUDLY if anyone reintroduces `-flags +low_delay`
// while "tidying" the args - it would re-throttle the decode. The
// format-level `-fflags +nobuffer` (S6-04) is the one we KEEP; it does
// not gate threading.
func TestBuildFFmpegArgs_NoCodecLowDelay(t *testing.T) {
	args := buildFFmpegArgs(EncodeSpec{Width: 800, Height: 1280, FPS: 12, Quality: 6})
	joined := strings.Join(args, " ")

	// Kept: format-level demuxer "don't buffer" (does not gate threading).
	if !strings.Contains(joined, "-fflags +nobuffer") {
		t.Errorf("missing -fflags +nobuffer (format-level, S6-04); args=%v", args)
	}
	// Removed in S2-16: codec-level low_delay throttles multi-core decode.
	if strings.Contains(joined, "+low_delay") {
		t.Errorf("`-flags +low_delay` must NOT be present: S2-16 measured it disables multi-core decode threading and breaks realtime decode of the 1200x1600 source. args=%v", args)
	}
}

// TestBuildFFmpegArgs_NoWallclockEvenRate is the S3-01 canary (it inverts
// the S6-04 wallclock expectation, analogous to NoCodecLowDelay above).
//
// S6-04 used `-use_wallclock_as_timestamps 1` assuming the camera ran
// ~15-17 fps. S3-01 measured a constant 30 fps and found wallclock turned
// the bursty pipe-arrival times into clumpy PTS, so the fps filter emitted
// clumps that overran the encoder input queue at 15-25 fps (12 was the
// prior ceiling). `-r 30` before `-i` gives an even 1/30 PTS base instead;
// `-threads 4` lets the 1200x1600 decode use all cores. Fails LOUDLY if
// wallclock returns or the rate/threads args go missing. See D-0003.
func TestBuildFFmpegArgs_NoWallclockEvenRate(t *testing.T) {
	args := buildFFmpegArgs(EncodeSpec{Width: 800, Height: 1280, FPS: 20, Quality: 6})
	joined := strings.Join(args, " ")
	iInput := strings.Index(joined, "-i pipe:0")
	if iInput < 0 {
		t.Fatalf("no -i pipe:0 in args=%v", args)
	}

	// Removed in S3-01: arrival-wallclock PTS caused bursty sampling.
	if strings.Contains(joined, "-use_wallclock_as_timestamps") {
		t.Errorf("`-use_wallclock_as_timestamps` must NOT be present: S3-01 replaced it with -r 30 (even sampling). args=%v", args)
	}
	// -r 30 present and BEFORE -i (applies to the raw H.264 demuxer).
	if iRate := strings.Index(joined, "-r 30"); iRate < 0 || iRate > iInput {
		t.Errorf("`-r 30` must be present and precede -i pipe:0; args=%v", args)
	}
	// -threads 4 present and BEFORE -i (decode-side threading).
	if iThreads := strings.Index(joined, "-threads 4"); iThreads < 0 || iThreads > iInput {
		t.Errorf("`-threads 4` must be present and precede -i pipe:0; args=%v", args)
	}
}

func TestCheckFFmpeg_MissingBinaryReturnsError(t *testing.T) {
	err := CheckFFmpeg("definitely-not-ffmpeg-on-any-path-99999")
	if err == nil {
		t.Fatal("expected error for missing ffmpeg path")
	}
	if !strings.Contains(err.Error(), "ffmpeg check") {
		t.Errorf("error should mention 'ffmpeg check'; got %v", err)
	}
}

// indexOf returns the position of s in args, or -1.
func indexOf(args []string, s string) int {
	for i, a := range args {
		if a == s {
			return i
		}
	}
	return -1
}

// max for tests; Go 1.21 has built-in max but we keep portable.
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
