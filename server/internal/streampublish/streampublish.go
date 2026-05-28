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

// StreamPublisher is the edge-side push surface the side-channel calls
// when the cloud asks for a stream.
//
//   StartPublish - begin pushing streamID to cloudWhipURL, presenting
//                  publishToken in the WHIP Authorization header.
//   StopPublish  - stop pushing streamID.
//
// Implementations MUST NOT block: StartPublish/StopPublish should kick
// off or signal asynchronous work and return promptly, so a slow or
// dead cloud never stalls the side-channel read loop or the local LAN
// path (Grundregel: the cloud is additive).
type StreamPublisher interface {
	StartPublish(streamID, publishToken, cloudWhipURL string)
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
func (n *Noop) StartPublish(streamID, publishToken, cloudWhipURL string) {
	n.log.Info("stream publish (no-op): would start WHIP push",
		"stream_id", streamID,
		"cloud_whip_url", cloudWhipURL,
		"has_token", publishToken != "",
	)
}

// StopPublish logs the intent.
func (n *Noop) StopPublish(streamID string) {
	n.log.Info("stream publish (no-op): would stop WHIP push", "stream_id", streamID)
}
