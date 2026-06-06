// Package streampublish defines the contract between carvilon and the
// stream layer for pushing a viewer's live video to the cloud. This is
// the Vertrags-Naht handed to the streaming-server: carvilon owns the
// transport, the trigger and the authorisation (the side-channel), and
// the stream layer implements StreamPublisher to perform the actual
// WHIP push.
//
// carvilon depends ONLY on this interface, never on pion / a WHIP
// client directly (adapter doctrine). The real implementation lives in
// the private streaming-server and is adapted in later wiring; the
// public build uses the no-op default.
package streampublish

import "log/slog"

// ICEServer is a carvilon-owned ICE/TURN server descriptor (the standard
// RTCIceServer shape). It is deliberately NOT pion's webrtc.ICEServer:
// this package lives in the public build, and importing pion here would
// break the open-core boundary (public build = 0 pion imports). The
// carvilon_stream-tagged edge naht translates this to webrtc.ICEServer
// when it calls the real WHIP client; the cloud closure mints these from
// the TURN shared secret and the side-channel carries them on
// request_publish.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// ICEResult is the outcome of the side-channel request_ice/ice_servers RPC:
// the cloud-minted subscriber ICE servers plus the public WHEP base URL the
// cloud advertises (scheme+host+port, no path; empty when the cloud has no
// public WHEP listener -> the edge falls back to its interim base). It lives
// here, the neutral contract package both the side-channel and the HTTP layer
// already import, so neither has to import the other and no pion type crosses
// the seam.
type ICEResult struct {
	Servers     []ICEServer
	WHEPBaseURL string
}

// StreamStartBundle is the JSON contract a remote subscriber needs to open a
// WHEP subscription: the public WHEP URL, a sid-bound egress token, the
// stream id (viewer MAC), the ICE servers, and the bundle lifetime. It is
// returned by BOTH the LAN edge endpoint (GET /webviewer/stream-start) and
// the cloud control endpoint (GET /stream-start on the signal host). Defined
// ONCE here - the neutral package the HTTP layer and the side-channel
// assembler both import - so the wire form has a single definition even
// though two sites fill it (drift guard). (Saison 19-11)
type StreamStartBundle struct {
	WHEPURL     string      `json:"whep_url"`
	EgressToken string      `json:"egress_token"`
	StreamID    string      `json:"stream_id"`
	ICEServers  []ICEServer `json:"ice_servers"`
	ExpiresIn   int         `json:"expires_in"`
	// EdgeWHEPURL is the LAN-direct WHEP URL (Saison 19-35). Present only
	// when the edge LAN-WHEP is active and the edge LAN IP is known; the
	// app tries it first (HTTP, same LAN) and falls back to WHEPURL (the
	// cloud path). NOTE: it carries the stream PROFILE NAME, not the MAC -
	// the edge keys /whep by profile (like /offer ?src=), unlike the cloud
	// (/whep/<mac>). omitempty so the cloud-only case stays byte-identical.
	EdgeWHEPURL string `json:"edge_whep_url,omitempty"`
}

// StreamPublisher is the edge-side push surface the side-channel calls
// when the cloud asks for a stream.
//
//	StartPublish - begin pushing streamID to cloudWhipURL, presenting
//	               publishToken in the WHIP Authorization header, using
//	               iceServers (cloud-minted short-lived TURN credentials;
//	               may be nil -> host candidates only) for the WebRTC
//	               path.
//	StopPublish  - stop pushing streamID.
//
// Implementations MUST NOT block: StartPublish/StopPublish should kick
// off or signal asynchronous work and return promptly, so a slow or
// dead cloud never stalls the side-channel read loop or the local LAN
// path (Grundregel: the cloud is additive).
type StreamPublisher interface {
	StartPublish(streamID, publishToken, cloudWhipURL string, iceServers []ICEServer)
	StopPublish(streamID string)
}

// Noop is the public-build default: it logs the calls and pushes
// nothing. The real WHIP client lives in the stream layer.
type Noop struct {
	log *slog.Logger
}

// NewNoop builds a Noop. A nil logger falls back to slog.Default().
func NewNoop(log *slog.Logger) *Noop {
	if log == nil {
		log = slog.Default()
	}
	return &Noop{log: log.With("component", "streampublish-noop")}
}

// StartPublish logs the intent. The publish_token is never logged
// verbatim (only its presence) to keep it out of the log trail.
func (n *Noop) StartPublish(streamID, publishToken, cloudWhipURL string, iceServers []ICEServer) {
	n.log.Info("stream publish (no-op): would start WHIP push",
		"stream_id", streamID,
		"cloud_whip_url", cloudWhipURL,
		"has_token", publishToken != "",
		"ice_servers", len(iceServers),
	)
}

// StopPublish logs the intent.
func (n *Noop) StopPublish(streamID string) {
	n.log.Info("stream publish (no-op): would stop WHIP push", "stream_id", streamID)
}
