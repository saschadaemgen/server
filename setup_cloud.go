package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"

	"github.com/pion/logging"
	"github.com/pion/turn/v4"
	"github.com/pion/webrtc/v4"

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

	// --- TURN (S3): in-process relay, soft-gated on TURNPublicIP. ---
	//
	// TURNPublicIP is the REAL public IP the relay announces as its relay
	// address (NOT a 172.x docker bridge - that was the ICE-befund noise).
	// Empty -> TURN is OFF: only WHIP/WHEP run and the cloud peers keep
	// empty ICEServers (host-candidate only, today's behaviour).
	TURNPublicIP string
	// TURNSharedSecret is the HMAC shared secret for the RFC-TURN-REST
	// ephemeral credentials. Required when TURN is on. NEVER logged.
	TURNSharedSecret []byte
	// TURNRealm - empty -> "carvilon".
	TURNRealm string
	// TURNUDPPort - 0 -> 3478. The UDP relay is the CGNAT workhorse.
	TURNUDPPort int
	// TURNTLSPort - 0 -> 5349. Reserved: v1 runs the UDP relay only; the
	// TLS listener (turns:) is a follow-up. The field is accepted now so
	// the embedding module can wire its env once.
	TURNTLSPort int

	// turnListenPacket is an UNEXPORTED test seam: when set it replaces the
	// real UDP bind so a test does not bind a fixed port. nil ->
	// net.ListenPacket. Invisible to external importers.
	turnListenPacket func(network, address string) (net.PacketConn, error)
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

	// TURN (S3): build the in-process relay + a per-peer ephemeral-
	// credential minter for the cloud peers' ICEServers. Soft-gated: with
	// no public IP, turnSrv + iceServers are nil and the peers stay
	// host-candidate only (today's behaviour).
	turnSrv, iceServers, err := setupTURN(opts, logger)
	if err != nil {
		return nil, nil, err
	}

	srv, err := whip.New(whip.Config{
		Addr:       opts.Addr,
		CertFile:   opts.CertFile,
		KeyFile:    opts.KeyFile,
		HMACKey:    opts.HMACKey,
		Hub:        hub,
		Logger:     logger,
		ICEServers: iceServers, // nil when TURN is off -> empty ICEServers
	})
	if err != nil {
		if turnSrv != nil {
			_ = turnSrv.Close()
		}
		return nil, nil, fmt.Errorf("stream: cloud whip server: %w", err)
	}

	var once sync.Once
	shutdown := func() error {
		// Idempotent. The WHIP server drains itself on ctx cancel; the only
		// OS resource to release is the TURN relay (turn.NewServer has no
		// ctx-aware Run, only Close), so the embedding module's ctx-end
		// teardown closes it here. No-op when TURN is off.
		once.Do(func() {
			if turnSrv != nil {
				_ = turnSrv.Close()
			}
		})
		return nil
	}

	return srv, shutdown, nil
}

// setupTURN builds the in-process pion/turn relay and a per-peer ephemeral
// REST-credential minter for the cloud peers' ICEServers. It is
// soft-gated: an empty TURNPublicIP returns (nil, nil, nil) so WHIP/WHEP
// run host-candidate-only, exactly as before TURN existed.
//
// Lifetime note (for the embedding module): turn.NewServer starts serving
// immediately and has no ListenAndServe(ctx); the caller stops it via the
// shutdown func (turnSrv.Close) at ctx end - that is why SetupCloudInProcess
// threads turnSrv into its shutdown.
func setupTURN(opts CloudSetupOptions, logger *log.Logger) (*turn.Server, func() ([]webrtc.ICEServer, error), error) {
	if opts.TURNPublicIP == "" {
		logger.Printf("stream: TURN disabled (no public IP); cloud peers stay host-candidate only")
		return nil, nil, nil
	}
	relayIP := net.ParseIP(opts.TURNPublicIP)
	if relayIP == nil {
		return nil, nil, fmt.Errorf("stream: TURNPublicIP %q is not a valid IP", opts.TURNPublicIP)
	}
	if len(opts.TURNSharedSecret) == 0 {
		return nil, nil, errors.New("stream: TURNSharedSecret is required when TURN is enabled")
	}
	realm := opts.TURNRealm
	if realm == "" {
		realm = "carvilon"
	}
	udpPort := opts.TURNUDPPort
	if udpPort == 0 {
		udpPort = 3478
	}

	listenPacket := opts.turnListenPacket
	if listenPacket == nil {
		listenPacket = net.ListenPacket
	}
	udpConn, err := listenPacket("udp", fmt.Sprintf(":%d", udpPort))
	if err != nil {
		return nil, nil, fmt.Errorf("stream: TURN UDP listen :%d: %w", udpPort, err)
	}

	// Discard pion's TURN logging so nothing about auth attempts (which
	// carry the ephemeral username) ever reaches a log sink.
	lf := logging.NewDefaultLoggerFactory()
	lf.Writer = io.Discard

	turnSrv, err := turn.NewServer(turn.ServerConfig{
		Realm:         realm,
		LoggerFactory: lf,
		AuthHandler:   turn.LongTermTURNRESTAuthHandler(string(opts.TURNSharedSecret), lf.NewLogger("turn")),
		PacketConnConfigs: []turn.PacketConnConfig{{
			PacketConn: udpConn,
			RelayAddressGenerator: &turn.RelayAddressGeneratorStatic{
				RelayAddress: relayIP,   // the announced relay address (public IP)
				Address:      "0.0.0.0", // bind address for the relay sockets
			},
		}},
	})
	if err != nil {
		_ = udpConn.Close()
		return nil, nil, fmt.Errorf("stream: TURN server: %w", err)
	}

	// Per-peer minter: fresh ephemeral creds each call (they expire), no
	// secret stored beyond this closure.
	publicIP, secret := opts.TURNPublicIP, opts.TURNSharedSecret
	minter := func() ([]webrtc.ICEServer, error) {
		user, pass, gerr := TURNCredentials(secret, "carvilon", DefaultTURNCredentialTTL)
		if gerr != nil {
			return nil, gerr
		}
		return TURNICEServers(publicIP, udpPort, user, pass), nil
	}

	logger.Printf("stream: TURN relay on :%d (realm=%q, relay-ip=%s)", udpPort, realm, opts.TURNPublicIP)
	return turnSrv, minter, nil
}
