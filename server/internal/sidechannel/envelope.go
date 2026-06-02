// Package sidechannel implements the Saison-17 edge<->cloud control
// link: a single mTLS WebSocket between the RPi (edge, which dials
// out from behind CGNAT) and the VPS (cloud, which listens). In this
// iteration the link only proves it is alive via an application-level
// ping/pong; the cargo schema (start_publish / stop_publish / ...)
// arrives in a later briefing.
//
// Grundregel (non-negotiable): the cloud link is ADDITIVE. The local
// LAN subsystems (mock viewers, doorbell hub, HTTP server, stream)
// work without any internet connection. Nothing in this package may
// block or fail the edge: the client runs in its own goroutine and a
// cloud outage only triggers reconnect attempts.
package sidechannel

import (
	"carvilon.local/server/internal/streampublish"
	"carvilon.local/server/internal/turnstore"
)

// Envelope is the JSON wire frame exchanged in both directions. Both
// sides dispatch on the "type" discriminator; adding a new message
// type is one more switch branch. Cargo fields are optional and only
// meaningful for the matching type (omitempty keeps ping/pong tiny).
type Envelope struct {
	Type string `json:"type"`

	// StreamID correlates a publish request/start/stop. It is the
	// viewer/mock MAC - the stable per-viewer identity the doorbellhub
	// already routes by and the edge can authorise against. Empty for
	// ping/pong.
	StreamID string `json:"stream_id,omitempty"`

	// PublishToken is issued by carvilon (edge) and carried on
	// start_publish; the stream-edge presents it on the WHIP push.
	PublishToken string `json:"publish_token,omitempty"`

	// Reason explains a stop_publish (see Reason* constants).
	Reason string `json:"reason,omitempty"`

	// ICEServers carries cloud-minted short-lived TURN credentials on
	// request_publish (cloud -> edge), so the edge can hand them to the
	// WHIP client for the WebRTC media path through CGNAT. Empty/nil ->
	// host candidates only (the pre-TURN behaviour, no break). Only
	// meaningful on request_publish: the cloud mints them (it holds the
	// shared secret), the edge never does.
	ICEServers []streampublish.ICEServer `json:"ice_servers,omitempty"`

	// TURNEvent carries one TURN lifecycle/auth event on a turn_event
	// frame (cloud -> edge), so the edge persists the relay history for
	// the /a/turn admin menu. Only the masked address is present; the
	// cloud drops the raw IP before sending (Saison 18-10).
	TURNEvent *turnstore.Event `json:"turn_event,omitempty"`

	// TURNStats carries a periodic live snapshot on a turn_stats frame
	// (cloud -> edge): the relay's allocation count + client set plus a
	// static config view. The edge shows it with a "Stand vor Xs"
	// freshness based on the receive time, not GeneratedAt.
	TURNStats *turnstore.Snapshot `json:"turn_stats,omitempty"`
}

// Message types.
const (
	// TypePing is sent by the edge as proof of life; the cloud
	// answers with TypePong.
	TypePing = "ping"
	// TypePong is the cloud's reply to a ping.
	TypePong = "pong"

	// TypeRequestPublish (cloud -> edge): a remote WHEP subscriber is
	// waiting; the cloud asks the edge to start pushing StreamID. The
	// anchor (a remote viewer) appears on the cloud side, so the cloud
	// triggers and the edge answers - the edge stays lazy.
	TypeRequestPublish = "request_publish"
	// TypeStartPublish (edge -> cloud): the edge accepted the push,
	// carries the carvilon-issued PublishToken for StreamID.
	TypeStartPublish = "start_publish"
	// TypeStopPublish (edge -> cloud): the edge stopped pushing
	// StreamID (Reason says why).
	TypeStopPublish = "stop_publish"

	// TypeTURNEvent (cloud -> edge): one TURN lifecycle/auth event for
	// the edge to persist (carries TURNEvent). The relay lives on the
	// cloud; the admin UI + SQLite live on the edge, so the telemetry
	// flows the same direction as request_publish.
	TypeTURNEvent = "turn_event"
	// TypeTURNStats (cloud -> edge): a periodic live snapshot for the
	// admin live-stats panel (carries TURNStats).
	TypeTURNStats = "turn_stats"
)

// stop_publish reasons.
const (
	ReasonNoSubscribers = "no_subscribers"
	ReasonError         = "error"
	ReasonCancelled     = "cancelled"
)
