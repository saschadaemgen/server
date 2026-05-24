// Package streams owns the contract between the carvilon HTTP
// layer and whichever video backend is wired (the transitional
// REST client today; carvilon-streaming-server via build tag
// carvilon_stream). The interface itself is defined in backend.go.
//
// Saison 14-01 first cut: the package targeted go2rtc directly
// (the Client type in client.go).
// Saison 15-01: the public surface is now the StreamBackend
// interface plus the structured Profile + Camera types here.
// Saison 15-25: the stream-server's profile schema settled at 11
// snake_case fields shared between GET and PUT, plus a new
// `encryption` field (tls / srtp). Profile here mirrors that wire
// shape one-to-one so the admin write path can round-trip GET
// payloads back through PUT without key renames.
package streams

// Profile is one configured stream entry as the admin UI and the
// stream-server REST API see it. JSON tags match the stream-
// server wire shape exactly: snake_case, no extras, no aliases.
// GET /api/profiles and PUT /api/profiles/{name} both consume
// this struct verbatim - the stream-server's DisallowUnknownFields
// rejects anything else.
//
// Name is the key the operator picks (e.g. "intercom_esp"); it
// doubles as the ?src= query parameter when consuming the
// profile via the backend's stream paths.
//
// CameraID points at the Protect-side camera the profile is
// sourced from.
//
// Quality / Usage / Description are operator-facing labels.
// Usage is "esp" or "browser" today.
//
// Codec is one of "h264_passthrough" / "mjpeg" / "h264_cbp".
// Width / Height / FPS apply to mjpeg + h264_cbp; EncodeQuality
// is the -q:v for mjpeg or the CRF for h264_cbp.
//
// Encryption picks the transport for the consumer-side stream:
// "tls" wraps the transport channel only (sufficient on the
// LAN), "srtp" additionally encrypts the media payload (for
// Internet / cloud consumers). The stream-server treats this as
// part of the pull key, so two profiles for the same camera with
// different encryption values open separate pulls.
//
// The /api/profiles response does NOT carry a live consumer
// count today; that surface is being clarified separately with
// the stream-chat and will land in a follow-up briefing.
type Profile struct {
	Name          string `json:"name"`
	CameraID      string `json:"camera_id"`
	Quality       string `json:"quality"`
	Usage         string `json:"usage"`
	Description   string `json:"description"`
	Codec         string `json:"codec"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	FPS           int    `json:"fps"`
	EncodeQuality int    `json:"encode_quality"`
	Encryption    string `json:"encryption"`
}
