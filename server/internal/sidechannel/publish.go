package sidechannel

import (
	"log/slog"

	"carvilon.local/server/internal/streampublish"
)

// TokenIssuer mints a publish token for a stream. publishtoken.Issuer
// satisfies it structurally; the interface lives here so this package
// need not import publishtoken (the wiring connects the two).
type TokenIssuer interface {
	Issue(streamID string) (string, error)
}

// EdgePublisher is the edge-side glue for the publish control flow. On
// request_publish from the cloud it authorises the stream, issues a
// publish token, sends start_publish back, and kicks the
// StreamPublisher. It also drives StopPublish.
//
// It holds no connection of its own: Send is wired to Client.Send (the
// single serialised writer), which is non-blocking and drops on a down
// link. So this whole path is decoupled from the local LAN flow - the
// Grundregel holds, a cloud outage cannot stall the edge.
type EdgePublisher struct {
	// Authorize reports whether carvilon knows streamID (a viewer row
	// / running mock exists behind the MAC). nil means allow-all
	// (not recommended outside tests).
	Authorize func(streamID string) bool
	// Issuer mints the publish token. Required.
	Issuer TokenIssuer
	// Publisher performs the real (or no-op) WHIP push. Required.
	Publisher streampublish.StreamPublisher
	// CloudWhipURL is the static cloud ingress the stream-edge pushes
	// to. Configured edge-side (env), not carried per frame.
	CloudWhipURL string
	// Send writes a frame on the side-channel (wired to Client.Send).
	Send func(Envelope)
	Log  *slog.Logger
}

func (e *EdgePublisher) logger() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

// HandleRequestPublish is the Client.OnRequestPublish callback:
// authorise, issue, start_publish, StartPublish. Any failure declines
// silently (logged) - the cloud simply gets no start_publish and can
// retry. It never returns an error and never blocks the caller beyond
// the (fast) token issue.
func (e *EdgePublisher) HandleRequestPublish(streamID string) {
	log := e.logger()
	if streamID == "" {
		log.Warn("request_publish with empty stream_id, ignoring")
		return
	}
	if e.Authorize != nil && !e.Authorize(streamID) {
		log.Warn("request_publish for unknown stream_id, declining", "stream_id", streamID)
		return
	}
	token, err := e.Issuer.Issue(streamID)
	if err != nil {
		log.Error("publish token issue failed, declining", "stream_id", streamID, "err", err)
		return
	}
	if e.Send != nil {
		e.Send(Envelope{Type: TypeStartPublish, StreamID: streamID, PublishToken: token})
	}
	// Kick the (async, non-blocking) WHIP push.
	e.Publisher.StartPublish(streamID, token, e.CloudWhipURL)
	log.Info("edge accepted publish request", "stream_id", streamID)
}

// StopPublish stops a stream: tell the cloud, then the publisher.
// Called when the local cause ends (no subscriber, error, cancel).
func (e *EdgePublisher) StopPublish(streamID, reason string) {
	if e.Send != nil {
		e.Send(Envelope{Type: TypeStopPublish, StreamID: streamID, Reason: reason})
	}
	e.Publisher.StopPublish(streamID)
	e.logger().Info("edge stopped publish", "stream_id", streamID, "reason", reason)
}
