//go:build carvilon_stream

// Saison 17-08 / 18-04 / 18-06: runtime wiring of the in-process WHIP/WHEP
// cloud stream server plus the in-process TURN relay. Compiled ONLY in the
// carvilon_stream build; the public build never sees it, so
// startInProcessCloudStream stays nil and runCloud skips the whole path
// (the open-core boundary holds). It is the cloud mirror of
// stream_inprocess_carvilon_stream.go (the edge seam).
package main

import (
	"context"
	"errors"
	"fmt"
	stdlog "log"
	"log/slog"
	"os"

	"github.com/pion/webrtc/v4"

	"carvilon.local/stream"

	"carvilon.local/server/internal/config"
	"carvilon.local/server/internal/sidechannel"
	"carvilon.local/server/internal/streampublish"
)

// init assigns the runtime slot declared in backend_default.go.
func init() {
	startInProcessCloudStream = func(
		ctx context.Context,
		log *slog.Logger,
		cfg config.Config,
		sc *sidechannel.Server,
	) (func(), error) {
		if !cfg.CloudStreamInProcessConfigured() {
			// Soft hint moved here from S18-05's ValidateCloud: TURN needs
			// the WHIP/WHEP server to relay for, so TURN-without-stream is a
			// no-op relay. Warn (do not fail) so a pure side-channel /
			// public deployment stays valid.
			if cfg.CloudTURNConfigured() {
				log.Warn("TURN is configured but the cloud in-process stream is not " +
					"(CARVILON_WHIP_CERT/_KEY unset); the TURN relay has nothing to relay for " +
					"and is NOT started. Running side-channel only.")
			} else {
				log.Info("in-process cloud stream not configured; running side-channel only " +
					"(set CARVILON_WHIP_CERT + CARVILON_WHIP_KEY to enable)")
			}
			return func() {}, nil
		}

		// The HMAC key is validated up front by ValidateCloud whenever the
		// cloud stream is configured; decode it here for the verifier.
		hmacKey, err := cfg.DecodePublishTokenHMACKey()
		if err != nil {
			return nil, fmt.Errorf("cloud in-process stream: publish-token HMAC key: %w", err)
		}

		// TURN (S18-06): mint short-lived ICE credentials per
		// request_publish. The side-channel Server calls this minter when it
		// asks an edge to publish; the long-term shared secret stays
		// cloud-side, only the short-lived credentials cross to the edge.
		// Independent soft gate (S18-05): no TURN config -> host-only ICE.
		if cfg.CloudTURNConfigured() {
			turnSecret := []byte(cfg.TURNSharedSecret)
			publicIP, udpPort, tlsPort := cfg.TURNPublicIP, cfg.TURNUDPPort, cfg.TURNTLSPort
			sc.SetICEMinter(func(streamID string) []streampublish.ICEServer {
				user, pass, mErr := stream.TURNCredentials(turnSecret, "carvilon", stream.DefaultTURNCredentialTTL)
				if mErr != nil {
					log.Error("turn credential mint failed; request_publish will carry host-only ICE", "err", mErr)
					return nil
				}
				// tlsPort 0 -> TURNICEServers omits the turns: leg (TLS off).
				return fromPionICE(stream.TURNICEServers(publicIP, udpPort, tlsPort, user, pass))
			})
			log.Info("in-process TURN relay configured", "udp_port", udpPort, "tls_port", tlsPort, "realm", cfg.TURNRealm)
		} else {
			log.Info("TURN not configured; request_publish carries host-only ICE " +
				"(set CARVILON_TURN_PUBLIC_IP + CARVILON_TURN_SHARED_SECRET to enable the relay)")
		}

		srv, shutdown, err := stream.SetupCloudInProcess(stream.CloudSetupOptions{
			Addr:             cfg.WhipListen, // empty -> stream defaults to :8444
			CertFile:         cfg.WhipCert,
			KeyFile:          cfg.WhipKey,
			HMACKey:          hmacKey,
			TURNPublicIP:     cfg.TURNPublicIP, // empty -> stream soft-gates TURN off
			TURNSharedSecret: []byte(cfg.TURNSharedSecret),
			TURNRealm:        cfg.TURNRealm,
			TURNUDPPort:      cfg.TURNUDPPort,
			TURNTLSPort:      cfg.TURNTLSPort,
			Logger:           stdlog.New(os.Stderr, "whip: ", stdlog.LstdFlags|stdlog.Lmsgprefix),
		})
		if err != nil {
			return nil, err
		}

		// SetupCloudInProcess builds but does NOT listen (same contract as
		// the edge). ListenAndServe binds the WHIP/WHEP TLS endpoint and the
		// TURN relay and blocks until ctx is cancelled, then drains; run it
		// in a goroutine. Its failure only logs - the side-channel keeps
		// running (Grundregel).
		go func() {
			if err := srv.ListenAndServe(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Error("in-process cloud stream server stopped", "err", err)
			}
		}()

		return func() { _ = shutdown() }, nil
	}
}

// fromPionICE converts the stream layer's pion ICE servers to carvilon's
// transport-neutral type for the side-channel frame. The pion Credential
// is an interface{}; the TURN-REST mint always sets it to a string.
func fromPionICE(in []webrtc.ICEServer) []streampublish.ICEServer {
	if len(in) == 0 {
		return nil
	}
	out := make([]streampublish.ICEServer, 0, len(in))
	for _, s := range in {
		cred, _ := s.Credential.(string)
		out = append(out, streampublish.ICEServer{
			URLs:       s.URLs,
			Username:   s.Username,
			Credential: cred,
		})
	}
	return out
}
