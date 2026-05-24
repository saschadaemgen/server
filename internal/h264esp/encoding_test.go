package h264esp

import (
	"strings"
	"testing"

	"carvilon.local/stream/internal/profile"
)

func TestEncodeSpec_Validate(t *testing.T) {
	good := EncodeSpec{Width: 800, Height: 1280, FPS: 15, Quality: 26}
	if err := good.Validate(); err != nil {
		t.Errorf("good spec failed validate: %v", err)
	}

	for _, c := range []struct {
		name string
		spec EncodeSpec
		want string
	}{
		{"zero width", EncodeSpec{Width: 0, Height: 1280, FPS: 15, Quality: 26}, "Width"},
		{"huge width", EncodeSpec{Width: 99999, Height: 1280, FPS: 15, Quality: 26}, "Width"},
		{"zero height", EncodeSpec{Width: 800, Height: 0, FPS: 15, Quality: 26}, "Height"},
		{"zero fps", EncodeSpec{Width: 800, Height: 1280, FPS: 0, Quality: 26}, "FPS"},
		{"huge fps", EncodeSpec{Width: 800, Height: 1280, FPS: 200, Quality: 26}, "FPS"},
		{"crf -1", EncodeSpec{Width: 800, Height: 1280, FPS: 15, Quality: -1}, "Quality"},
		{"crf 99", EncodeSpec{Width: 800, Height: 1280, FPS: 15, Quality: 99}, "Quality"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.spec.Validate()
			if err == nil {
				t.Fatalf("expected error for %v", c.spec)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err %q should mention %q", err.Error(), c.want)
			}
		})
	}
}

// TestOutputArgs_LocksBriefingFlags is the canary test for the wire-
// shape requirements from the S6-02 briefing. If a future refactor
// drops one of these flags the ESP loses its decode invariants — let
// the test fail loud and bring the briefing back into the room.
func TestOutputArgs_LocksBriefingFlags(t *testing.T) {
	spec := EncodeSpec{Width: 800, Height: 1280, FPS: 15, Quality: 26}
	args := spec.OutputArgs()
	joined := strings.Join(args, " ")

	// Required flags (briefing FEST):
	mustHave := []string{
		"-c:v libx264",
		"-profile:v baseline", // Constrained Baseline
		"-bf 0",               // no B-frames
		"-refs 1",             // single reference
		"-g 15",               // GoP = fps = 15
		"-keyint_min 15",      // matching GoP minimum
		"-sc_threshold 0",     // no scene-change keyframes
		"-tune zerolatency",
		"-preset ultrafast",
		// S6-04: undo sliced-threads from -tune zerolatency. Without
		// this, libx264 emits multiple slices per frame and the
		// splitter's "one VCL NAL = one AU" contract inflates the
		// frame counter.
		"-x264-params sliced-threads=0:slices=1",
		"-bsf:v dump_extra=freq=keyframe", // SPS/PPS before every IDR
		"-f h264",                          // Annex-B output
		// S6-13: fps filter first, then scale (single -vf chain). -r
		// at output is gone — see internal/mjpeg/encoding.go for the
		// full rationale (was causing stdin-pipe throttling that
		// surfaced as "encoder input channel full" drops).
		"-vf fps=15,scale=800:1280",
		"-crf 26",
		"-an",
	}
	for _, want := range mustHave {
		if !strings.Contains(joined, want) {
			t.Errorf("OutputArgs missing %q\nfull args: %v", want, args)
		}
	}

	// Forbidden flags (briefing: NO AUDs):
	mustNotHave := []string{
		"aud=insert",
		"h264_metadata",
	}
	for _, bad := range mustNotHave {
		if strings.Contains(joined, bad) {
			t.Errorf("OutputArgs contains forbidden %q\nfull args: %v", bad, args)
		}
	}
}

func TestOutputArgs_GoPMatchesFPS(t *testing.T) {
	// GoP = 1 second invariant: -g and -keyint_min must equal FPS.
	for _, fps := range []int{8, 12, 15, 25, 30} {
		spec := EncodeSpec{Width: 320, Height: 240, FPS: fps, Quality: 26}
		args := spec.OutputArgs()
		joined := strings.Join(args, " ")
		want := "-g " + itoa(fps)
		if !strings.Contains(joined, want) {
			t.Errorf("FPS=%d: args missing %q\nargs=%v", fps, want, args)
		}
		want = "-keyint_min " + itoa(fps)
		if !strings.Contains(joined, want) {
			t.Errorf("FPS=%d: args missing %q\nargs=%v", fps, want, args)
		}
	}
}

// TestOutputArgs_FpsFilterFirstAndNoBareR is the S6-13 canary for
// h264esp — same shape as the mjpeg counterpart. The fps filter must
// precede scale in the -vf chain, and the standalone `-r N` at output
// must NOT be present (it caused stdin-pipe throttling that produced
// motion-streak drops on the ESP).
func TestOutputArgs_FpsFilterFirstAndNoBareR(t *testing.T) {
	args := EncodeSpec{Width: 800, Height: 1280, FPS: 15, Quality: 26}.OutputArgs()
	joined := strings.Join(args, " ")

	want := "-vf fps=15,scale=800:1280"
	if !strings.Contains(joined, want) {
		t.Errorf("missing %q (fps filter must precede scale).\nargs=%v", want, args)
	}
	for i, a := range args {
		if a == "-r" {
			t.Errorf("found `-r` at args[%d] — S6-13 removed it from h264esp too. args=%v", i, args)
		}
	}
}

func TestSpecFromProfile_AcceptsH264CBP(t *testing.T) {
	p := profile.Profile{
		Name: "h264_cbp", CameraID: "c", Quality: profile.QualityHigh,
		Usage: profile.UsageESP, Codec: profile.CodecH264CBP,
		Width: 800, Height: 1280, FPS: 15, EncodeQuality: 26,
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("test profile invalid: %v", err)
	}
	spec, err := SpecFromProfile(p)
	if err != nil {
		t.Fatalf("SpecFromProfile: %v", err)
	}
	if spec.Width != 800 || spec.Height != 1280 || spec.FPS != 15 || spec.Quality != 26 {
		t.Errorf("spec = %+v", spec)
	}
}

func TestSpecFromProfile_RejectsOtherCodecs(t *testing.T) {
	for _, codec := range []profile.Codec{profile.CodecH264Passthrough, profile.CodecMJPEG} {
		p := profile.Profile{
			Name: "x", CameraID: "c", Quality: profile.QualityHigh,
			Usage: profile.UsageESP, Codec: codec,
			Width: 800, Height: 1280, FPS: 15, EncodeQuality: 6,
		}
		_, err := SpecFromProfile(p)
		if err == nil {
			t.Errorf("SpecFromProfile(%s) should error", codec)
		}
	}
}

func itoa(n int) string {
	// Small int → string without importing strconv for the test.
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+(n%10))) + digits
		n /= 10
	}
	if neg {
		digits = "-" + digits
	}
	return digits
}
