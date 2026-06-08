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

// ProfileStats is one per-profile row from GET /stream/stats, keyed by
// profile name. It mirrors the stream-server's stats.ProfileSnapshot
// field-for-field (JSON tags identical). Clients is the live consumer
// count pulling THIS profile (the per-profile number the admin column
// shows). SourceFrames / SourceFPS are how many upstream access units
// reached the encoder and at what rate - the gap to AvgFPS localises
// frame loss (camera vs encoder vs wire).
//
// The JSON decoder ignores any field the stream-server adds beyond
// this struct (no DisallowUnknownFields on the read side).
type ProfileStats struct {
	Profile        string  `json:"profile"`
	Codec          string  `json:"codec"`
	Clients        int     `json:"clients"`
	FramesSent     int64   `json:"frames_sent"`
	FramesDropped  int64   `json:"frames_dropped"`
	BytesSent      int64   `json:"bytes_sent"`
	AvgFPS         float64 `json:"avg_fps"`
	AvgBitrateKbps float64 `json:"avg_bitrate_kbps"`
	SourceFrames   int64   `json:"source_frames"`
	SourceFPS      float64 `json:"source_fps"`
}

// GlobalStats is the server-wide aggregate from GET /stream/stats
// (stats.GlobalSnapshot). TranscoderCPUPercent is present only when the
// stream-server has a CPU sampler wired.
type GlobalStats struct {
	Clients              int     `json:"clients"`
	FramesSentTotal      int64   `json:"frames_sent_total"`
	BytesSentTotal       int64   `json:"bytes_sent_total"`
	TranscoderCPUPercent float64 `json:"transcoder_cpu_percent"`
}

// ClientStats is one connected consumer from GET /stream/stats
// (stats.ClientSnapshot). Times are RFC3339 strings. RemoteAddr drives
// the admin device-type heuristic (ESP / Browser / Loopback).
type ClientStats struct {
	ID             uint64  `json:"id"`
	Profile        string  `json:"profile"`
	Codec          string  `json:"codec"`
	RemoteAddr     string  `json:"remote_addr"`
	ConnectedAt    string  `json:"connected_at"`
	UptimeSec      float64 `json:"uptime_sec"`
	FramesSent     int64   `json:"frames_sent"`
	FramesDropped  int64   `json:"frames_dropped"`
	BytesSent      int64   `json:"bytes_sent"`
	AvgFPS         float64 `json:"avg_fps"`
	AvgBitrateKbps float64 `json:"avg_bitrate_kbps"`
	LastFrameAt    string  `json:"last_frame_at"`
}

// StreamStats is the full GET /stream/stats document
// (stats.Snapshot): the global aggregate, the per-profile map (active
// profiles only), and the flat per-client list. It is the data source
// for the admin streams dashboard (global band, per-profile rows,
// per-client detail). A profile absent from Profiles has zero
// consumers and is idle.
type StreamStats struct {
	GeneratedAt string                  `json:"generated_at"`
	Global      GlobalStats             `json:"global"`
	Profiles    map[string]ProfileStats `json:"profiles"`
	Clients     []ClientStats           `json:"clients"`
}
