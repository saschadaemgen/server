package stream

import (
	"context"
	"fmt"
	"log"

	"carvilon.local/stream/internal/streamhub"
	"carvilon.local/stream/internal/whip"
)

// CloudSetupOptions carries everything [SetupCloudInProcess] needs to
// stand up the in-process WHIP-ingress + WHEP-egress for the CLOUD role,
// for embedding in carvilon-server's runCloud (the carvilon_stream build).
//
// It is the cloud mirror of [EdgeSetupOptions], but for the cloud half:
// no camera source, just the public WHIP/WHEP TLS endpoint plus the
// publish-token verify key. The fan-out hub is built internally, so the
// embedding module never has to touch internal/streamhub.
type CloudSetupOptions struct {
	// Addr is the WHIP/WHEP TLS listen address (e.g. ":8444"). Empty ->
	// the whip package default (":8444").
	Addr string
	// CertFile / KeyFile are absolute paths to the server TLS cert/key
	// (PEM). Required. Their existence is checked lazily at serve time by
	// the TLS stack, NOT at construction.
	CertFile string
	KeyFile  string
	// HMACKey is the publish-token HMAC key, already hex-decoded - the
	// same key the edge signs publish tokens with, so the ingress can
	// verify them. Required, must be non-empty.
	HMACKey []byte
	// Logger - if nil, a default logger.
	Logger *log.Logger
}

// CloudServer is the run-handle returned by [SetupCloudInProcess]. The
// embedding module (carvilon-server runCloud, carvilon_stream build) runs
// it in a goroutine - `go srv.ListenAndServe(ctx)` - and cancels ctx to
// stop it, exactly the way the edge side runs the *Server.
//
// It is an INTERFACE, not the concrete internal/whip type, so the
// embedding module can name and store the handle without importing
// internal/whip (which is cross-module-unreachable). The concrete
// *whip.Server satisfies it.
type CloudServer interface {
	// ListenAndServe binds the WHIP/WHEP TLS endpoint and serves until ctx
	// is cancelled, then drains gracefully. It BLOCKS - run in a goroutine.
	ListenAndServe(ctx context.Context) error
}

// SetupCloudInProcess builds the in-process WHIP-ingress + WHEP-egress
// cloud stack (a [streamhub] fan-out fed by WHIP publishers, drained by
// WHEP subscribers) for embedding in carvilon-server's runCloud. It is the
// cloud mirror of [SetupEdgeInProcess].
//
// Listen-vs-setup line (identical to the edge side, so the embedding
// module wires both halves the same way): this builds but does NOT
// listen. The caller runs `go srv.ListenAndServe(ctx)` and cancels ctx to
// stop. The hub is built internally; the caller supplies only the TLS +
// token config.
//
// The returned shutdown is idempotent. Today it has nothing to release
// beyond the ctx-driven graceful drain inside ListenAndServe (the hub
// holds no OS resources and the server shuts itself down on ctx cancel);
// it exists for symmetry with the edge shutdown and as the future
// teardown hook. Config validation (cert/key present, HMAC non-empty, hub
// set) is delegated to [whip.New] and surfaced verbatim.
func SetupCloudInProcess(opts CloudSetupOptions) (CloudServer, func() error, error) {
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}

	// One fan-out hub per cloud process: WHIP publishers register here,
	// WHEP subscribers attach to the stored track.
	hub := streamhub.NewHub()

	srv, err := whip.New(whip.Config{
		Addr:     opts.Addr,
		CertFile: opts.CertFile,
		KeyFile:  opts.KeyFile,
		HMACKey:  opts.HMACKey,
		Hub:      hub,
		Logger:   logger,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("stream: cloud whip server: %w", err)
	}

	shutdown := func() error {
		// Idempotent no-op today: ListenAndServe drains the HTTP server on
		// ctx cancel, and the hub holds no OS resources. A plain return is
		// the honest mirror of the edge shutdown (which DOES own a registry
		// + store); kept so the embedding module wires teardown identically.
		return nil
	}

	return srv, shutdown, nil
}
