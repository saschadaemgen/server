//go:build carvilon_stream

// Saison 18-04: runtime wiring of the in-process WHIP/WHEP cloud stream
// server. Compiled ONLY in the carvilon_stream build; the public build
// never sees it, so startInProcessCloudStream stays nil and runCloud
// skips the whole path (the open-core boundary holds). It is the cloud
// mirror of stream_inprocess_carvilon_stream.go (the edge seam): same
// nil-default-plus-tagged-init() shape, same listen-vs-setup contract
// (SetupCloudInProcess builds but does not listen; we run ListenAndServe
// in a goroutine and ctx-cancel stops it).
package main

import (
	"context"
	"errors"
	"fmt"
	stdlog "log"
	"log/slog"
	"os"

	"carvilon.local/stream"

	"carvilon.local/server/internal/config"
)

// init assigns the runtime slot declared in backend_default.go.
func init() {
	startInProcessCloudStream = func(
		ctx context.Context,
		log *slog.Logger,
		cfg config.Config,
	) (func(), error) {
		if !cfg.CloudStreamInProcessConfigured() {
			log.Info("in-process cloud stream not configured; running side-channel only " +
				"(set CARVILON_WHIP_CERT + CARVILON_WHIP_KEY to enable)")
			return func() {}, nil
		}

		// The HMAC key is validated up front by ValidateCloud whenever the
		// cloud stream is configured; decode it here for the verifier.
		hmacKey, err := cfg.DecodePublishTokenHMACKey()
		if err != nil {
			return nil, fmt.Errorf("cloud in-process stream: publish-token HMAC key: %w", err)
		}

		srv, shutdown, err := stream.SetupCloudInProcess(stream.CloudSetupOptions{
			Addr:     cfg.WhipListen, // empty -> stream defaults to :8444
			CertFile: cfg.WhipCert,
			KeyFile:  cfg.WhipKey,
			HMACKey:  hmacKey,
			Logger:   stdlog.New(os.Stderr, "whip: ", stdlog.LstdFlags|stdlog.Lmsgprefix),
		})
		if err != nil {
			return nil, err
		}

		// SetupCloudInProcess builds but does NOT listen (same contract as
		// the edge SetupEdgeInProcess). ListenAndServe binds the WHIP/WHEP
		// TLS endpoint and blocks until ctx is cancelled, then drains; run
		// it in a goroutine. Its failure only logs - the side-channel keeps
		// running (Grundregel).
		go func() {
			if err := srv.ListenAndServe(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Error("in-process cloud stream server stopped", "err", err)
			}
		}()

		return func() { _ = shutdown() }, nil
	}
}
