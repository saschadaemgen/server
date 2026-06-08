package sourcereg

import "testing"

// TestKeyString_PipelineAnnotation: the passthrough pipeline (the historical
// case, incl. empty) renders without a suffix so existing log lines stay
// byte-stable; only a non-passthrough pipeline is annotated.
func TestKeyString_PipelineAnnotation(t *testing.T) {
	base := Key{CameraID: "cam", Quality: "medium", Encryption: "tls"}
	if got := base.String(); got != "cam:medium/tls" {
		t.Errorf("empty pipeline String() = %q, want cam:medium/tls", got)
	}

	pt := base
	pt.Pipeline = "passthrough"
	if got := pt.String(); got != "cam:medium/tls" {
		t.Errorf("passthrough String() = %q, want unchanged cam:medium/tls", got)
	}

	re := base
	re.Pipeline = "reencode_shortgop"
	if got := re.String(); got != "cam:medium/tls [reencode_shortgop]" {
		t.Errorf("reencode String() = %q, want annotated", got)
	}
}

// TestHubFor_DistinctByPipeline: two pipelines on the same
// (camera, quality, encryption) get DISTINCT hubs, so the re-encode pull is
// separate from the passthrough pull in the map (while still costing one
// camera connection, because the re-encode source taps the passthrough hub).
func TestHubFor_DistinctByPipeline(t *testing.T) {
	reg, _ := newTestRegistry(t)
	pt := Key{CameraID: "cam", Quality: "medium", Encryption: "tls", Pipeline: "passthrough"}
	re := Key{CameraID: "cam", Quality: "medium", Encryption: "tls", Pipeline: "reencode_shortgop"}

	hpt := reg.HubFor(pt)
	hre := reg.HubFor(re)
	if hpt == hre {
		t.Fatal("passthrough and reencode pipelines must get distinct hubs")
	}
	if reg.HubFor(pt) != hpt {
		t.Error("HubFor(pt) must be stable across calls")
	}
	if !reg.Has(pt) || !reg.Has(re) {
		t.Error("both keys should be registered after HubFor")
	}
}
