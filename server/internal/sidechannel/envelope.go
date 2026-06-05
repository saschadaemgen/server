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

	// RequestID correlates a request_ice with its ice_servers reply: the
	// edge sets it on request_ice, the cloud mirrors it back on ice_servers.
	// Empty on every other frame type.
	RequestID string `json:"request_id,omitempty"`

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

	// ICEServers carries cloud-minted short-lived TURN credentials. The
	// cloud mints them (it holds the shared secret; the edge never does) and
	// the edge hands them to the WebRTC media path through CGNAT. Empty/nil
	// -> host candidates only (the pre-TURN behaviour, no break). Carried on
	// request_publish (for the WHIP publisher) AND on ice_servers (the reply
	// to a request_ice, for a remote WHEP subscriber - Saison 19).
	ICEServers []streampublish.ICEServer `json:"ice_servers,omitempty"`

	// ExpiresInSeconds is a credential lifetime in seconds carried on a reply
	// frame: on ice_servers (cloud -> edge) the TURN-cred TTL derived from the
	// cloud mint TTL; on bundle_reply (edge -> cloud) the egress-token TTL.
	ExpiresInSeconds int `json:"expires_in_seconds,omitempty"`

	// WHEPBaseURL is the public WHEP base URL the cloud advertises on an
	// ice_servers reply (cloud -> edge): scheme+host+port, NO path, e.g.
	// "https://<host>:8446". The edge appends "/whep/<mac>". Empty -> the edge
	// falls back to its interim base (derived from the cloud WHIP ingress
	// URL). Only meaningful on ice_servers. (Saison 19-08)
	WHEPBaseURL string `json:"whep_base_url,omitempty"`

	// Credential carries the raw viewer Bearer (device_token) on a
	// bundle_request (cloud -> edge): the cloud forwards what the remote
	// subscriber presented so the edge can resolve it to a MAC and mint an
	// egress token. Only meaningful on bundle_request. (Saison 19-11)
	Credential string `json:"credential,omitempty"`

	// MAC and EgressToken carry the edge's bundle_reply (edge -> cloud): the
	// resolved viewer MAC and the freshly minted, sid-bound egress token. The
	// cloud assembles the rest of the bundle (ICE + WHEP URL) itself. Both
	// empty when Error is set. Only meaningful on bundle_reply. (Saison 19-11)
	MAC         string `json:"mac,omitempty"`
	EgressToken string `json:"egress_token,omitempty"`
	// Error, when non-empty on a bundle_reply, signals the edge rejected the
	// request (auth failed / could not mint). It is a fixed, non-revealing
	// string - the concrete reason stays in the edge log - which the cloud
	// maps to a bare 401. Reused on http_reply (Saison 19-27): the edge could
	// not RUN the request at all (mechanism failure -> cloud 503); a normal
	// HTTP status (incl. 401) rides Status, not Error.
	Error string `json:"error,omitempty"`

	// --- Generic HTTP control relay (Saison 19-27) ---
	// http_request (cloud -> edge): the cloud forwards a remote app's raw
	// control call so the edge runs it through its OWN mux (requireViewerAuth
	// + the unchanged handler). http_reply (edge -> cloud): the captured HTTP
	// response. The edge stays the auth authority; the relay is a dumb pipe.

	// Method / Path / RawQuery describe the relayed request (http_request).
	// Path is the FULL path including any path params (e.g. the {door_id}
	// segment) so the edge mux matches its pattern.
	Method   string `json:"method,omitempty"`
	Path     string `json:"path,omitempty"`
	RawQuery string `json:"raw_query,omitempty"`
	// HTTPHeader is the CURATED header set: on http_request only Authorization
	// + Content-Type; on http_reply only Content-Type. Never Host/Cookie/etc.
	HTTPHeader map[string]string `json:"http_header,omitempty"`
	// Body is the request body (http_request) or response body (http_reply);
	// base64 on the wire (JSON []byte), capped (~64 KB) by the sender - the
	// side-channel is a control channel, not a data pipe.
	Body []byte `json:"body,omitempty"`
	// Status is the captured HTTP status on an http_reply (edge -> cloud).
	Status int `json:"status,omitempty"`

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

	// TypeRequestICE (edge -> cloud): the edge asks the cloud to mint a
	// fresh set of subscriber ICE servers (the cloud holds the TURN shared
	// secret; the edge never does). Carries RequestID (correlation) and
	// optionally StreamID (logging only - the creds are sid-agnostic).
	// Decoupled from request_publish on purpose (Saison 19 decision).
	TypeRequestICE = "request_ice"
	// TypeICEServers (cloud -> edge): the reply to request_ice, mirroring
	// RequestID and carrying ICEServers + ExpiresInSeconds. An empty
	// ICEServers list signals "no minter / mint failed" to the edge.
	TypeICEServers = "ice_servers"

	// TypeBundleRequest (cloud -> edge): the cloud relays a remote
	// subscriber's stream-start request. Carries RequestID + Credential (the
	// raw viewer Bearer). Only the edge can resolve the viewer and mint the
	// egress token; the cloud assembles the rest. The reverse-direction
	// mirror of request_ice (cloud waits, edge replies). (Saison 19-11)
	TypeBundleRequest = "bundle_request"
	// TypeBundleReply (edge -> cloud): the edge's answer, mirroring RequestID
	// and carrying MAC + EgressToken + ExpiresInSeconds, or Error when the
	// edge rejected (auth/mint). (Saison 19-11)
	TypeBundleReply = "bundle_reply"

	// TypeHTTPRequest (cloud -> edge): the cloud relays a remote app's raw
	// control call (Method/Path/RawQuery/HTTPHeader/Body) so the edge runs it
	// through its own mux unchanged. Generalisation of bundle_request, for the
	// /webviewer/* control family (reject/end-call/answer/unlock/fcm-token).
	// (Saison 19-27)
	TypeHTTPRequest = "http_request"
	// TypeHTTPReply (edge -> cloud): the edge's captured HTTP response
	// (Status/HTTPHeader/Body), or Error on a mechanism failure. (Saison 19-27)
	TypeHTTPReply = "http_reply"

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
