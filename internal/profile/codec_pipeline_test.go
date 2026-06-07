package profile

import "testing"

func TestCodecPipeline(t *testing.T) {
	cases := map[Codec]string{
		CodecH264Passthrough:      PipelinePassthrough,
		CodecMJPEG:                PipelinePassthrough,
		CodecH264CBP:              PipelinePassthrough,
		CodecH264ReencodeShortGOP: PipelineReencodeShortGOP,
		Codec("weird"):            PipelinePassthrough,
		Codec(""):                 PipelinePassthrough,
	}
	for c, want := range cases {
		if got := c.Pipeline(); got != want {
			t.Errorf("Codec(%q).Pipeline() = %q, want %q", c, got, want)
		}
	}
}

// TestValidate_ReencodeCodecNoDimsRequired: the short-GOP re-encode keeps the
// camera's resolution/fps, so (like passthrough) it must validate WITHOUT
// admin Width/Height/FPS/EncodeQuality.
func TestValidate_ReencodeCodecNoDimsRequired(t *testing.T) {
	p := Profile{
		Name: "intercom_4g", CameraID: "cam-1",
		Quality: QualityMedium, Usage: UsageBrowser,
		Codec: CodecH264ReencodeShortGOP,
	}
	if err := p.Validate(); err != nil {
		t.Errorf("reencode profile should validate without dimensions: %v", err)
	}
}

// TestValidate_MJPEGStillRequiresDims guards that scoping the dimension
// requirement to the to-spec transcodes did not loosen MJPEG/CBP.
func TestValidate_MJPEGStillRequiresDims(t *testing.T) {
	p := Profile{
		Name: "m", CameraID: "cam-1",
		Quality: QualityHigh, Usage: UsageESP, Codec: CodecMJPEG,
	}
	if err := p.Validate(); err == nil {
		t.Error("mjpeg without dimensions must still fail validation")
	}
}

func TestValidate_ReencodeCodecAccepted(t *testing.T) {
	p := Profile{
		Name: "x", CameraID: "cam-1",
		Quality: QualityMedium, Usage: UsageBrowser,
		Codec: Codec("totally_unknown_codec"),
	}
	if err := p.Validate(); err == nil {
		t.Error("an unknown codec must still be rejected")
	}
}
