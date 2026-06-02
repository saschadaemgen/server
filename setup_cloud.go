package stream

import (
	"context"
	"crypto/tls"
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
	// TURNTLSPort enables the TLS relay leg (turns:, TURN over TLS/TCP) on
	// that port when > 0 (S3; 5349 is the turns: standard port). 0 -> TLS
	// leg OFF (opt-in): only the UDP relay + STUN run. A TLS-on setup with
	// an unloadable cert is a hard error (no silent partial relay).
	TURNTLSPort int
	// TURNPublicHost is the public HOSTNAME the turns: leg is advertised on
	// (e.g. "turn.carvilon.com"), resolving to the relay's public IP. It
	// MUST match the TLS cert's SAN: pion verifies the turns: handshake
	// against the system root pool with ServerName = the URL host, so a
	// turns: on a bare IP with a private-CA cert is rejected (the S3
	// turns-befund). stun:/turn: stay on TURNPublicIP (they need no cert).
	// Empty -> no turns: line is advertised (an IP-turns would not verify),
	// even when TURNTLSPort > 0.
	TURNPublicHost string
	// TURNTLSCertFile / TURNTLSKeyFile are the cert/key for the TLS relay
	// leg, SEPARATE from the WHIP certs: the WHIP ingress stays on the
	// private cloudca CA (internal path) while the public turns: leg uses a
	// publicly-trusted cert (e.g. Let's Encrypt for TURNPublicHost) that an
	// arbitrary client's system pool trusts. Empty -> fall back to
	// CertFile/KeyFile (the WHIP certs; dev / single-cert setups).
	TURNTLSCertFile string
	TURNTLSKeyFile  string

	// OnTURNEvent, when non-nil, is called for every TURN lifecycle/auth
	// event (allocation created/deleted/error, auth verdict) so the
	// embedding module can persist a real event history (the master wires
	// it to SQLite). OPTIONAL: nil -> no callback. Open-core: the event
	// carries only stdlib types (no pion/net.Addr), the mieter IP both raw
	// and masked, and NEVER the shared secret or the credential password.
	// pion may invoke it concurrently from its own goroutines; the callback
	// must be safe for concurrent use.
	OnTURNEvent func(TURNEvent)

	// turnListenPacket / turnListenTLS are UNEXPORTED test seams: when set
	// they replace the real UDP bind / TLS listener so a test need not bind
	// a fixed port or load a real cert. nil -> net.ListenPacket /
	// LoadX509KeyPair+tls.Listen. Invisible to external importers.
	turnListenPacket func(network, address string) (net.PacketConn, error)
	turnListenTLS    func(address string) (net.Listener, error)
}

// CloudServer is the run-handle returned by [SetupCloudInProcess]. The
// embedding module (carvilon-server runCloud, carvilon_stream build) runs
// it in a goroutine - `go srv.ListenAndServe(ctx)` - and cancels ctx to
// stop it, exactly the way the edge side runs the *Server.
//
// It is an INTERFACE, not the concrete internal/whip type, so the
// embedding module can name and store the handle without importing
// internal/whip (which is cross-module-unreachable). The unexported
// cloudServer wrapper satisfies it (it delegates ListenAndServe to the WHIP
// server and answers TURNStats from the in-process relay).
type CloudServer interface {
	// ListenAndServe binds the WHIP/WHEP TLS endpoint and serves until ctx
	// is cancelled, then drains gracefully. It BLOCKS - run in a goroutine.
	ListenAndServe(ctx context.Context) error

	// TURNStats returns a point-in-time snapshot of the in-process TURN
	// relay for the admin to poll (analogous to /stream/stats). When TURN
	// is soft-gated off it returns TURNStats{Enabled: false}. Safe to call
	// concurrently with the relay's own goroutines.
	TURNStats() TURNStats
}

// cloudServer is the concrete CloudServer: it delegates ListenAndServe to
// the WHIP/WHEP server and answers TURNStats from the in-process relay plus
// the EventHandler-maintained client set. turnSrv/clients are nil when TURN
// is soft-gated off (TURNStats then reports Enabled:false).
type cloudServer struct {
	whip    *whip.Server
	turnSrv *turn.Server
	clients *turnClientSet
}

func (c *cloudServer) ListenAndServe(ctx context.Context) error {
	return c.whip.ListenAndServe(ctx)
}

func (c *cloudServer) TURNStats() TURNStats {
	if c.turnSrv == nil {
		return TURNStats{Enabled: false}
	}
	stats := TURNStats{Enabled: true, AllocationCount: c.turnSrv.AllocationCount()}
	if c.clients != nil {
		stats.Clients = c.clients.snapshot()
	}
	return stats
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
	turnSrv, clientSet, iceServers, err := setupTURN(opts, logger)
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

	return &cloudServer{whip: srv, turnSrv: turnSrv, clients: clientSet}, shutdown, nil
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
func setupTURN(opts CloudSetupOptions, logger *log.Logger) (*turn.Server, *turnClientSet, func() ([]webrtc.ICEServer, error), error) {
	if opts.TURNPublicIP == "" {
		logger.Printf("stream: TURN disabled (no public IP); cloud peers stay host-candidate only")
		return nil, nil, nil, nil
	}
	relayIP := net.ParseIP(opts.TURNPublicIP)
	if relayIP == nil {
		return nil, nil, nil, fmt.Errorf("stream: TURNPublicIP %q is not a valid IP", opts.TURNPublicIP)
	}
	if len(opts.TURNSharedSecret) == 0 {
		return nil, nil, nil, errors.New("stream: TURNSharedSecret is required when TURN is enabled")
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
		return nil, nil, nil, fmt.Errorf("stream: TURN UDP listen :%d: %w", udpPort, err)
	}

	// A fresh static relay generator per config (announces the public IP,
	// NOT a docker bridge). Separate instances keep the UDP and TLS legs
	// from sharing generator state.
	mkRelay := func() turn.RelayAddressGenerator {
		return &turn.RelayAddressGeneratorStatic{
			RelayAddress: relayIP,   // the announced relay address (public IP)
			Address:      "0.0.0.0", // bind address for the relay sockets
		}
	}

	// S3 Posten A: optional TLS relay leg (turns:) on TURNTLSPort, reusing
	// the WHIP cert/key. tlsPort == 0 -> off (opt-in). A TLS-on setup with
	// an unloadable cert is a HARD error (no silent partial relay) - and it
	// is the SAME cert WHIP requires, so a broken one is already fatal.
	var listenerConfigs []turn.ListenerConfig
	tlsPort := opts.TURNTLSPort
	if tlsPort > 0 {
		tlsLn, lerr := turnTLSListener(opts, tlsPort)
		if lerr != nil {
			_ = udpConn.Close()
			return nil, nil, nil, fmt.Errorf("stream: TURN TLS listen :%d: %w", tlsPort, lerr)
		}
		listenerConfigs = []turn.ListenerConfig{{
			Listener:              tlsLn,
			RelayAddressGenerator: mkRelay(),
		}}
	}

	// Discard pion's TURN logging so nothing about auth attempts (which
	// carry the ephemeral username) ever reaches a log sink.
	lf := logging.NewDefaultLoggerFactory()
	lf.Writer = io.Discard

	// S3 telemetry: the EventHandler maintains the live client set behind
	// TURNStats and forwards lifecycle/auth events to opts.OnTURNEvent (when
	// set). It reads pion's net.Addr only at the boundary (addrPair) and
	// emits open-core TURNEvents - no pion/net type and no secret escapes.
	clientSet := newTURNClientSet()
	eventHandler := newTURNEventHandler(clientSet, opts.OnTURNEvent)

	turnSrv, err := turn.NewServer(turn.ServerConfig{
		Realm:         realm,
		LoggerFactory: lf,
		AuthHandler:   turn.LongTermTURNRESTAuthHandler(string(opts.TURNSharedSecret), lf.NewLogger("turn")),
		EventHandler:  eventHandler,
		PacketConnConfigs: []turn.PacketConnConfig{{
			PacketConn:            udpConn,
			RelayAddressGenerator: mkRelay(),
		}},
		ListenerConfigs: listenerConfigs, // nil -> UDP-only (TLS leg off)
	})
	if err != nil {
		_ = udpConn.Close()
		return nil, nil, nil, fmt.Errorf("stream: TURN server: %w", err)
	}

	// Per-peer minter: fresh ephemeral creds each call (they expire), no
	// secret stored beyond this closure.
	publicIP, secret, turnsHost := opts.TURNPublicIP, opts.TURNSharedSecret, opts.TURNPublicHost
	minter := func() ([]webrtc.ICEServer, error) {
		user, pass, gerr := TURNCredentials(secret, "carvilon", DefaultTURNCredentialTTL)
		if gerr != nil {
			return nil, gerr
		}
		return TURNICEServers(publicIP, turnsHost, udpPort, tlsPort, user, pass), nil
	}

	switch {
	case tlsPort > 0 && turnsHost != "":
		logger.Printf("stream: TURN relay on udp:%d + tls:%d (turns host=%s, realm=%q, relay-ip=%s)",
			udpPort, tlsPort, turnsHost, realm, opts.TURNPublicIP)
	case tlsPort > 0:
		logger.Printf("stream: TURN relay on udp:%d + tls:%d listening, but TURNPublicHost is empty -> turns: NOT advertised (realm=%q, relay-ip=%s)",
			udpPort, tlsPort, realm, opts.TURNPublicIP)
	default:
		logger.Printf("stream: TURN relay on udp:%d (realm=%q, relay-ip=%s); TLS leg disabled (TURNTLSPort=0)",
			udpPort, realm, opts.TURNPublicIP)
	}
	return turnSrv, clientSet, minter, nil
}

// turnTLSListener builds the TLS listener for the turns: leg. It uses the
// SEPARATE TURNTLSCertFile/KeyFile (the publicly-trusted host cert that an
// arbitrary client's system pool trusts) when both are set, else falls
// back to the WHIP CertFile/KeyFile (dev / single-cert setups). The WHIP
// ingress itself is untouched - it keeps its own (private-CA) cert.
//
// Eager load - a broken cert fails setup, surfaced by the caller as a hard
// error. The test seam opts.turnListenTLS bypasses the cert load + bind.
func turnTLSListener(opts CloudSetupOptions, tlsPort int) (net.Listener, error) {
	addr := fmt.Sprintf(":%d", tlsPort)
	if opts.turnListenTLS != nil {
		return opts.turnListenTLS(addr)
	}
	certFile, keyFile := opts.TURNTLSCertFile, opts.TURNTLSKeyFile
	if certFile == "" || keyFile == "" {
		certFile, keyFile = opts.CertFile, opts.KeyFile // fall back to the WHIP certs
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load cert/key: %w", err)
	}
	return tls.Listen("tcp", addr, &tls.Config{Certificates: []tls.Certificate{cert}})
}
