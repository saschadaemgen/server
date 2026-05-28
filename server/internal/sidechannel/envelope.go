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

// Envelope is the JSON wire frame exchanged in both directions. It is
// deliberately minimal: a single "type" discriminator. Future cargo
// messages add fields here and further "type" values; both sides
// already dispatch on a type switch so adding a branch is the only
// change a new message type needs.
type Envelope struct {
	Type string `json:"type"`
}

// Message types. Only ping/pong exist in this iteration.
const (
	// TypePing is sent by the edge as proof of life; the cloud
	// answers with TypePong.
	TypePing = "ping"
	// TypePong is the cloud's reply to a ping.
	TypePong = "pong"
)
