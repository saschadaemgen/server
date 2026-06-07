package stream

import (
	"testing"

	"carvilon.local/stream/internal/profile"
)

func reencodeProfile(name, cam string) profile.Profile {
	return profile.Profile{
		Name: name, CameraID: cam, Quality: profile.QualityMedium,
		Usage: profile.UsageBrowser, Codec: profile.CodecH264ReencodeShortGOP,
	}
}

// TestSourceKeyFor_Pipeline: the per-pull key carries the codec's pipeline,
// so a re-encode profile and a passthrough profile on the SAME camera resolve
// to DIFFERENT hubs.
func TestSourceKeyFor_Pipeline(t *testing.T) {
	pass := passthroughProfile("intercom_web", "cam-1")
	re := reencodeProfile("intercom_4g", "cam-1")
	mj := mjpegProfile("mjpeg_bal", "cam-1")
	srv, _ := trackTestServer(t, []profile.Profile{pass, re, mj})

	if got := srv.sourceKeyFor(pass).Pipeline; got != profile.PipelinePassthrough {
		t.Errorf("passthrough pipeline = %q, want %q", got, profile.PipelinePassthrough)
	}
	if got := srv.sourceKeyFor(mj).Pipeline; got != profile.PipelinePassthrough {
		t.Errorf("mjpeg pipeline = %q, want %q", got, profile.PipelinePassthrough)
	}
	if got := srv.sourceKeyFor(re).Pipeline; got != profile.PipelineReencodeShortGOP {
		t.Errorf("reencode pipeline = %q, want %q", got, profile.PipelineReencodeShortGOP)
	}
	if srv.sourceKeyFor(re) == srv.sourceKeyFor(pass) {
		t.Error("reencode and passthrough on the same camera must not share a source key")
	}
}

// TestTrackForStream_ReencodeCodecAccepted: the cloud-push gate accepts the
// re-encode codec (it is WebRTC-egressable H.264). The test factory returns a
// fake source for any key, so this exercises the SERVER gate + key wiring
// without spawning a real v4l2m2m ffmpeg.
func TestTrackForStream_ReencodeCodecAccepted(t *testing.T) {
	re := reencodeProfile("intercom_4g", "cam-1")
	srv, _ := trackTestServer(t, []profile.Profile{re})

	track, stop, err := srv.TrackForStream("intercom_4g")
	if err != nil {
		t.Fatalf("TrackForStream(reencode) = %v, want success", err)
	}
	if track == nil {
		t.Fatal("nil track")
	}
	stop()
}
