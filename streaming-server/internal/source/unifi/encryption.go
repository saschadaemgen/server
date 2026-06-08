package unifi

import "net/url"

// stripEnableSrtp removes the UniFi-specific `enableSrtp` query parameter
// from a Protect-API-returned RTSPS URL, leaving everything else
// (scheme, host, path, other query parameters, fragment) untouched.
//
// The Protect integration API attaches `?enableSrtp` to every URL it
// returns. With it, the UniFi RTSP server promotes the media to SRTP
// (RFC 3711) with the key delivered via MIKEY in the SDP — a path the
// rest of the Go ecosystem has not converged on for these cameras
// (go2rtc Issue #81). Without it, UniFi delivers plain RTP through the
// TLS tunnel that the `rtsps://` scheme already provides, which our
// depacketizer can decode end-to-end.
//
// If the URL fails to parse, the input is returned unchanged — a parse
// failure is reported later by gortsplib with a clearer error site.
func stripEnableSrtp(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := u.Query()
	if !q.Has("enableSrtp") {
		return rawURL
	}
	q.Del("enableSrtp")
	u.RawQuery = q.Encode()
	return u.String()
}
