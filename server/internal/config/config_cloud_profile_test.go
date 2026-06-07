package config

import "testing"

// TestCloudPublishProfileOr proves the S19-44 cloud-publish routing decision:
// empty CloudStreamProfile -> the fallback (today's per-viewer profile, NO
// break); set -> the configured re-encode profile.
func TestCloudPublishProfileOr(t *testing.T) {
	c := Config{}
	if got := c.CloudPublishProfileOr("intercom_android"); got != "intercom_android" {
		t.Errorf("empty CloudStreamProfile -> %q, want fallback intercom_android (no break)", got)
	}
	c.CloudStreamProfile = "h264_reencode_shortgop"
	if got := c.CloudPublishProfileOr("intercom_android"); got != "h264_reencode_shortgop" {
		t.Errorf("set -> %q, want h264_reencode_shortgop", got)
	}
}
