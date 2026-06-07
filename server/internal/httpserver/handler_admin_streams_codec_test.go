package httpserver

import "testing"

// TestValidStreamCodec_AllowsReencodeShortGOP: the admin codec allow-list
// accepts the S19-44 4G re-encode codec so the profile is creatable/visible
// in the admin UI; unrelated codecs stay rejected.
func TestValidStreamCodec_AllowsReencodeShortGOP(t *testing.T) {
	for _, c := range []string{"mjpeg", "h264_cbp", "h264_passthrough", "h264_reencode_shortgop"} {
		if !validStreamCodec(c) {
			t.Errorf("validStreamCodec(%q) = false, want true", c)
		}
	}
	for _, c := range []string{"", "h265", "vp9", "reencode"} {
		if validStreamCodec(c) {
			t.Errorf("validStreamCodec(%q) = true, want false", c)
		}
	}
}
