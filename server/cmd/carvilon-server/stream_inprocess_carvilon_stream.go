//go:build carvilon_stream

// Saison 17-08: runtime wiring of the in-process stream server. This
// file is compiled ONLY in the carvilon_stream build; the public build
// never sees it, so startInProcessStream stays nil and runEdge skips
// the whole path (the open-core boundary holds).
//
// It replaces the old init()-at-build-time commercialBackend assignment
// (removed in the S17-08 repair commit): the in-process stack needs
// runtime config and a lifecycle context, which an init() cannot give.
package main

import (
	"context"
	"errors"
	"log/slog"

	"carvilon.local/stream"

	"carvilon.local/server/internal/config"
	"carvilon.local/server/internal/streampublish"
	"carvilon.local/server/internal/streams"
	"carvilon.local/server/internal/viewermanager"
)

// init assigns the runtime slot declared in backend_default.go.
func init() {
	startInProcessStream = func(
		ctx context.Context,
		log *slog.Logger,
		cfg config.Config,
		viewerMgr *viewermanager.Manager,
	) (streams.StreamBackend, streampublish.StreamPublisher, func(), error) {
		if !cfg.StreamInProcessConfigured() {
			log.Info("in-process stream not configured; using the HTTP stream backend instead " +
				"(set CARVILON_STREAM_NVR_HOST/_API_KEY/_DB_PATH/_ADDR + CARVILON_STREAM_BACKEND_URL)")
			return nil, nil, nil, nil
		}

		opts := stream.EdgeSetupOptions{
			NVRHost:     cfg.StreamNVRHost,
			APIKey:      cfg.StreamAPIKey,
			DBPath:      cfg.StreamDBPath,
			Addr:        cfg.StreamAddr,
			BaseURL:     cfg.StreamBackendURL,
			FFmpegPath:  cfg.StreamFFmpegPath,
			EnableMJPEG: cfg.StreamEnableMJPEG,
		}
		// Typed Encryption without importing the internal profile
		// package: an untyped string constant is assignable to the named
		// profile.Encryption field, so the public-clean boundary holds.
		// Empty -> SetupEdgeInProcess defaults it to tls.
		switch cfg.StreamEncryption {
		case "srtp":
			opts.Encryption = "srtp"
		case "tls":
			opts.Encryption = "tls"
		}

		srv, backend, shutdown, err := stream.SetupEdgeInProcess(opts)
		if err != nil {
			return nil, nil, nil, err
		}

		// D2: SetupEdgeInProcess does NOT listen on its own. Run(ctx)
		// serves the local MJPEG/Offer HTTP path on Addr and self-shuts-
		// down on ctx.Done(); without it the loopback stream path is
		// dead. Its failure only logs - the edge core keeps running.
		go func() {
			if err := srv.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Error("in-process stream server stopped", "err", err)
			}
		}()

		wrapped := streams.NewCarvilonStreamBackend(backend)

		// S17-08 D returns the WHIP cloud-push publisher here (built from
		// srv.TrackForStream + viewerMgr). For now nil keeps the Noop
		// publisher; the local MJPEG/Offer path is already live above.
		return wrapped, nil, func() { _ = shutdown() }, nil
	}
}
