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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	stdlog "log"
	"log/slog"
	"os"

	"github.com/pion/webrtc/v4"

	"carvilon.local/stream"
	"carvilon.local/stream/whipclient"

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
			// EnableStats flips on the stream-server's per-client
			// throughput tracker so /stream/stats reports real numbers
			// (ESP-diagnostics). Stats is left nil on purpose: with
			// EnableStats true + Stats nil, SetupEdgeInProcess builds the
			// registry internally - carvilon cannot import the internal
			// stats package, so it must not set Stats itself.
			EnableStats: true,
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

		// WHIP cloud-push publisher. Best-effort: if the Mini-CA is
		// missing/unreadable the publisher degrades to nil (Noop) and the
		// local MJPEG/Offer path is unaffected. The combined shutdown
		// closes the WHIP client first (terminating live pushes), then
		// the stream stack.
		cleanup := func() { _ = shutdown() }
		var publisher streampublish.StreamPublisher
		if client, perr := buildWHIPClient(ctx, log, cfg, srv, viewerMgr); perr != nil {
			log.Error("whip publisher init failed; cloud push disabled (local stream unaffected)", "err", perr)
		} else {
			publisher = client
			cleanup = func() { _ = client.Close(); _ = shutdown() }
		}

		return wrapped, publisher, cleanup, nil
	}
}

// buildWHIPClient constructs the WHIP cloud-push client. The TrackSource
// resolves a streamID (viewer MAC) to its stream profile and opens an
// in-process TrackForStream track - the same shared camera pull the
// local viewers use. The TLS config pins the cloud WHIP server against
// the Mini-CA (the same ca.crt the side-channel client trusts).
func buildWHIPClient(
	ctx context.Context,
	log *slog.Logger,
	cfg config.Config,
	srv *stream.Server,
	viewerMgr *viewermanager.Manager,
) (*whipclient.Client, error) {
	tlsCfg, err := whipTLSConfig(cfg.SidechannelCACert)
	if err != nil {
		return nil, err
	}
	trackSource := func(streamID string) (webrtc.TrackLocal, func(), error) {
		info, err := viewerMgr.GetViewerInfo(ctx, streamID)
		if err != nil {
			return nil, nil, err
		}
		// TrackForStream rejects non-h264_passthrough profiles (e.g. an
		// ESP MJPEG viewer) with an error; the whipclient worker logs it
		// and tears down - no panic, the local flow is untouched.
		return srv.TrackForStream(info.ResolveStreamProfile())
	}
	return whipclient.New(whipclient.Config{
		TrackSource: trackSource,
		TLSConfig:   tlsCfg,
		Logger:      stdlog.New(os.Stderr, "whip: ", stdlog.LstdFlags|stdlog.Lmsgprefix),
	})
}

// whipTLSConfig builds a tls.Config that trusts only the Mini-CA in
// caPath (RootCAs). The cloud WHIP server presents the whip-server cert
// signed by that CA; standard verification applies (no skip-verify).
func whipTLSConfig(caPath string) (*tls.Config, error) {
	if caPath == "" {
		return nil, errors.New("whip: CARVILON_SIDECHANNEL_CA_CERT is required for the WHIP TLS connection")
	}
	pemBytes, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("whip: read ca cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("whip: ca cert %s has no usable certificates", caPath)
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}
