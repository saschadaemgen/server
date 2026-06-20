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
	"time"

	"github.com/pion/webrtc/v4"

	"carvilon.local/stream"

	"carvilon.local/server/internal/config"
	"carvilon.local/server/internal/sidechannel"
	"carvilon.local/server/internal/streampublish"
	"carvilon.local/server/internal/streamstore"
	"carvilon.local/server/internal/turnstore"
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

		// S18-16: egress-token key for the WHEP egress 401-verify.
		// Fail-closed: empty/invalid -> nil -> the stream rejects every
		// WHEP subscribe (401). The non-empty gate is required because
		// DecodeEgressTokenHMACKey errors on an empty key; a set-but-
		// invalid key is already rejected up front by ValidateCloud, so a
		// decode error here is defensive only.
		var egressKey []byte
		if cfg.EgressTokenHMACKey != "" {
			k, decErr := cfg.DecodeEgressTokenHMACKey()
			if decErr != nil {
				log.Error("egress token key invalid; WHEP egress stays fail-closed (401 for all)", "err", decErr)
			} else {
				egressKey = k
			}
		}

		// TURN (S18-06): mint short-lived ICE credentials per
		// request_publish. The side-channel Server calls this minter when it
		// asks an edge to publish; the long-term shared secret stays
		// cloud-side, only the short-lived credentials cross to the edge.
		// Independent soft gate (S18-05): no TURN config -> host-only ICE.
		if cfg.CloudTURNConfigured() {
			turnSecret := []byte(cfg.TURNSharedSecret)
			publicIP, udpPort, tlsPort := cfg.TURNPublicIP, cfg.TURNUDPPort, cfg.TURNTLSPort
			turnsHost := cfg.TURNPublicHost
			sc.SetICEMinter(func(streamID string) []streampublish.ICEServer {
				user, pass, mErr := stream.TURNCredentials(turnSecret, "carvilon", stream.DefaultTURNCredentialTTL)
				if mErr != nil {
					log.Error("turn credential mint failed; request_publish will carry host-only ICE", "err", mErr)
					return nil
				}
				// turns: is announced only when tlsPort > 0 AND turnsHost is
				// set; otherwise TURNICEServers returns stun + turn on the IP.
				return fromPionICE(stream.TURNICEServers(publicIP, turnsHost, udpPort, tlsPort, user, pass))
			}, int(stream.DefaultTURNCredentialTTL.Seconds()))
			log.Info("in-process TURN relay configured", "udp_port", udpPort, "tls_port", tlsPort, "realm", cfg.TURNRealm)
		} else {
			log.Info("TURN not configured; request_publish carries host-only ICE " +
				"(set CARVILON_TURN_PUBLIC_IP + CARVILON_TURN_SHARED_SECRET to enable the relay)")
		}

		// Saison 18-10: TURN telemetry forwarder. OnTURNEvent fires from
		// pion's goroutines; it drops the raw IP and enqueues on this
		// buffered channel so the relay never blocks on the side-channel
		// write. A drain goroutine (started below) does the send.
		turnEvents := make(chan turnstore.Event, 256)

		srv, shutdown, err := stream.SetupCloudInProcess(stream.CloudSetupOptions{
			Addr:             cfg.WhipListen, // empty -> stream defaults to :8444
			CertFile:         cfg.WhipCert,
			KeyFile:          cfg.WhipKey,
			HMACKey:          hmacKey,
			EgressHMACKey:    egressKey,        // empty -> WHEP egress fails closed (401)
			TURNPublicIP:     cfg.TURNPublicIP, // empty -> stream soft-gates TURN off
			TURNSharedSecret: []byte(cfg.TURNSharedSecret),
			TURNRealm:        cfg.TURNRealm,
			TURNUDPPort:      cfg.TURNUDPPort,
			TURNTLSPort:      cfg.TURNTLSPort,
			TURNPublicHost:   cfg.TURNPublicHost,
			TURNTLSCertFile:  cfg.TURNTLSCertFile,
			TURNTLSKeyFile:   cfg.TURNTLSKeyFile,
			// S19-07 Baustufe 2: separate public WHEP-egress listener + public
			// cert. Empty WHEPPublicAddr -> off; the :8444 cloudca WHIP/WHEP
			// listener stays untouched. ValidateCloud enforced host+cert+key
			// once the addr is set; the stream module returns the public base
			// via CloudServer.WHEPPublicBaseURL() for the edge to advertise.
			WHEPPublicAddr:     cfg.WHEPPublicAddr,
			WHEPPublicHost:     cfg.WHEPPublicHost,
			WHEPPublicCertFile: cfg.WHEPPublicCert,
			WHEPPublicKeyFile:  cfg.WHEPPublicKey,
			Logger:             stdlog.New(os.Stderr, "whip: ", stdlog.LstdFlags|stdlog.Lmsgprefix),
			OnTURNEvent: func(e stream.TURNEvent) {
				// Privacy: drop the raw IP here on the VPS; only the
				// masked form is enqueued and ever leaves this process.
				select {
				case turnEvents <- toTurnstoreEvent(e):
				default:
					log.Warn("turn event buffer full, dropping", "kind", e.Kind)
				}
			},
			// S18-11: cold-start WHEP trigger. A remote WHEP subscriber for
			// a stream with no active publisher asks the edge to publish
			// over the side-channel. The method value matches
			// RequestPublishFunc exactly; the WHEP request ctx flows
			// through, so a subscriber disconnect aborts the trigger.
			OnRequestPublish: sc.RequestPublish,
			// S20: when the LAST WHEP subscriber for a stream leaves, ask the
			// edge (over the side-channel) to stop its publish bridge so the
			// profile row falls back to idle. Fired in a goroutine so the WHEP
			// teardown path (a pion goroutine) never blocks on the side-channel
			// broadcast write. The symmetric counterpart to OnRequestPublish.
			OnLastSubscriberGone: func(streamID string) { go sc.RequestStop(ctx, streamID) },
		})
		if err != nil {
			return nil, err
		}

		// Saison 19-08: hand the cloud's public WHEP base to the side-channel
		// so every ice_servers reply advertises it (empty when the public WHEP
		// listener is off -> the edge uses its interim base). Set once here,
		// after the stream server is built, analogous to SetICEMinter above.
		sc.SetWHEPBaseURL(srv.WHEPPublicBaseURL())

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

		// Drain enqueued TURN events to the edge, decoupled from pion's
		// goroutines (Saison 18-10).
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case ev := <-turnEvents:
					sc.SendTURNEvent(ctx, ev)
				}
			}
		}()

		// Push a periodic live snapshot (+ config view) so the edge admin
		// shows current relay numbers with an honest "Stand vor Xs". The
		// snapshot carries Enabled=false when TURN is off, so the edge can
		// tell "cloud up, TURN off" from "cloud unreachable".
		go func() {
			ticker := time.NewTicker(turnSnapshotInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					sc.SendTURNStats(ctx, buildTURNSnapshot(srv.TURNStats(), cfg))
					// S20: egress consumer counts, pushed on the SAME tick right
					// next to the TURN snapshot - same side-channel, no new port,
					// no new secret. An empty snapshot (no streams) still arrives,
					// so the edge can tell "cloud up, 0 viewers" from "cloud down".
					sc.SendStreamStats(ctx, buildStreamSnapshot(srv.StreamStats()))
				}
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

// turnSnapshotInterval is how often the cloud pushes a live TURN
// snapshot to the edge. Kept small (Sascha: seconds, not minutes); the
// edge's stale threshold (turnstore.DefaultStaleAfter) is 3x this.
const turnSnapshotInterval = 10 * time.Second

// toTurnstoreEvent converts the stream layer's TURN event to carvilon's
// transport-neutral type, KEEPING ONLY the masked address. The raw IP
// (e.SrcAddr / e.DstAddr) is deliberately not copied, so it never
// leaves the VPS. Compiled only in the carvilon_stream build.
func toTurnstoreEvent(e stream.TURNEvent) turnstore.Event {
	out := turnstore.Event{
		Kind:      e.Kind,
		Time:      e.Time,
		SrcMasked: e.SrcAddrMasked,
		DstMasked: e.DstAddrMasked,
		Protocol:  e.Protocol,
		Username:  e.Username,
		Realm:     e.Realm,
		Err:       e.Err,
	}
	if e.AuthOK != nil {
		v := *e.AuthOK
		out.AuthOK = &v
	}
	return out
}

// buildTURNSnapshot assembles the live snapshot the cloud pushes to the
// edge: pion's live numbers plus the static config view the edge cannot
// see itself (the TURN config is cloud-side). Masked addresses only.
func buildTURNSnapshot(st stream.TURNStats, cfg config.Config) turnstore.Snapshot {
	snap := turnstore.Snapshot{
		Enabled:         st.Enabled,
		AllocationCount: st.AllocationCount,
		GeneratedAt:     time.Now(),
		STUNActive:      st.Enabled, // credential-less STUN rides the relay's UDP port
	}
	for _, c := range st.Clients {
		snap.Clients = append(snap.Clients, turnstore.Client{
			SrcMasked: c.SrcAddrMasked,
			Username:  c.Username,
			Since:     c.Since,
		})
	}
	if cfg.CloudTURNConfigured() {
		snap.UDPPort = cfg.TURNUDPPort
		snap.TLSPort = cfg.TURNTLSPort
		snap.Realm = cfg.TURNRealm
		snap.TURNSHost = cfg.TURNPublicHost
		snap.CredTTLSeconds = int(stream.DefaultTURNCredentialTTL.Seconds())
		switch {
		case cfg.TURNTLSPort == 0:
			snap.CertMode = "" // TLS relay leg off
		case cfg.TURNTLSCertFile != "":
			snap.CertMode = "separate"
		default:
			snap.CertMode = "shared"
		}
	}
	return snap
}

// buildStreamSnapshot assembles the live cloud-viewer snapshot the cloud
// pushes to the edge: the per-stream WHEP-subscriber (consumer) counts, the
// egress mirror of buildTURNSnapshot. GeneratedAt stamps the VPS send time;
// the edge shows it with a "Stand vor Xs" based on its OWN receive time, so a
// fresh snapshot with no streams ("cloud up, 0 viewers") stays distinguishable
// from a stale one ("cloud unreachable"). Only stdlib types cross the seam.
func buildStreamSnapshot(stats []stream.StreamStat) streamstore.Snapshot {
	snap := streamstore.Snapshot{GeneratedAt: time.Now()}
	for _, st := range stats {
		snap.Streams = append(snap.Streams, streamstore.Stat{
			StreamID:  st.StreamID,
			Consumers: st.Consumers,
		})
	}
	return snap
}
