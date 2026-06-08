// Saison 15-07 + Saison 17-08: public-side slots for the commercial
// stream integration. Both are declared here (NOT build-tagged) so the
// declarations are visible in BOTH builds; the carvilon_stream build
// assigns them at runtime (see stream_inprocess_carvilon_stream.go).
// The public build leaves them at their zero values, so it never
// imports carvilon.local/stream.

package main

import (
	"context"
	"log/slog"

	"carvilon.local/server/internal/config"
	"carvilon.local/server/internal/sidechannel"
	"carvilon.local/server/internal/streampublish"
	"carvilon.local/server/internal/streams"
	"carvilon.local/server/internal/turnstore"
	"carvilon.local/server/internal/viewermanager"
)

// commercialBackend is nil in the public build. The carvilon_stream
// build assigns it (via startInProcessStream below) to the in-process
// streaming-server's StreamBackend. main() reads this slot first and
// falls back to the transitional go2rtc client / streams.Unconfigured()
// when it stays nil.
var commercialBackend streams.StreamBackend

// startInProcessStream is nil in the public build. The carvilon_stream
// build assigns it (init() in stream_inprocess_carvilon_stream.go) to a
// function that boots the in-process stream server and returns:
//   - its StreamBackend (for the commercialBackend slot / MJPEG+Offer),
//   - a StreamPublisher (the WHIP cloud-push client; nil until S17-08 D),
//   - a shutdown func, and an error.
//
// runEdge calls it only when non-nil, so the public build never reaches
// any carvilon.local/stream code. The signature references only public
// carvilon types (turnstore.Writer is pure carvilon, no pion), so this
// declaration compiles in both builds.
//
// iceWriter is the edge-local sink for the whipclient's ICE-state
// events (Saison 18-10); the tagged build points whipclient.OnICEState
// at it. Nil disables that leg.
var startInProcessStream func(
	ctx context.Context,
	log *slog.Logger,
	cfg config.Config,
	viewerMgr *viewermanager.Manager,
	iceWriter *turnstore.Writer,
) (streams.StreamBackend, streampublish.StreamPublisher, func(), error)

// startInProcessCloudStream is nil in the public build. The
// carvilon_stream build assigns it (init() in
// stream_inprocess_cloud_carvilon_stream.go) to a function that boots the
// in-process WHIP-ingress + WHEP-egress cloud stream server and returns a
// shutdown func. runCloud calls it only when non-nil, so the public build
// never reaches any carvilon.local/stream code. The signature references
// only public carvilon types, so this declaration compiles in both
// builds. It is the cloud mirror of startInProcessStream (the edge seam).
var startInProcessCloudStream func(
	ctx context.Context,
	log *slog.Logger,
	cfg config.Config,
	sc *sidechannel.Server,
) (func(), error)
