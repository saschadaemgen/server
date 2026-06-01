package sidechannel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"carvilon.local/server/internal/streampublish"
)

// ServerOptions configures the cloud-side listener.
type ServerOptions struct {
	// ListenAddr is the bind address, e.g. ":8443". Ignored when
	// Listener is set.
	ListenAddr string

	// CACertPath is the CA that must have signed the presented client
	// certificate. Clients without a cert signed by this CA are
	// rejected at the TLS handshake.
	CACertPath string
	// ServerCert / ServerKey are the cloud server's own cert+key.
	ServerCert string
	ServerKey  string

	Log *slog.Logger

	// Listener, when non-nil, is served instead of binding ListenAddr.
	// Tests pass a 127.0.0.1:0 listener; production leaves it nil.
	Listener net.Listener

	// OnStartPublish / OnStopPublish are optional hooks fired when the
	// edge reports it began / stopped pushing a stream. The server
	// already tracks the active-stream set internally; these let the
	// wiring log or a test observe. nil in the simplest setup.
	OnStartPublish func(streamID, publishToken string)
	OnStopPublish  func(streamID, reason string)
}

// Server is the cloud-side side-channel listener. mTLS
// (RequireAndVerifyClientCert) is the only authentication: whoever
// holds a client certificate signed by our CA (the RPi) is let in.
// It answers each ping with a pong.
type Server struct {
	opts      ServerOptions
	tlsConfig *tls.Config
	log       *slog.Logger
	connCount atomic.Int64

	mu            sync.Mutex
	conns         map[*serverConn]struct{}
	activeStreams map[string]struct{}
	// iceMinter, when set, mints the per-request TURN ICE servers the
	// cloud attaches to each request_publish frame. Set by the
	// carvilon_stream-tagged cloud closure (which holds the TURN shared
	// secret); nil in the public build -> request_publish carries no ICE
	// (host-only, the pre-TURN behaviour).
	iceMinter func(streamID string) []streampublish.ICEServer
}

// SetICEMinter installs the per-request TURN ICE-server minter. The
// carvilon_stream-tagged cloud closure calls this with a closure that
// mints short-lived credentials from the TURN shared secret; the public
// build never calls it. Set once before Run; guarded by the same mutex
// RequestPublish reads under.
func (s *Server) SetICEMinter(m func(streamID string) []streampublish.ICEServer) {
	s.mu.Lock()
	s.iceMinter = m
	s.mu.Unlock()
}

// serverConn is one accepted edge connection plus a write mutex.
// coder/websocket forbids concurrent writers, so RequestPublish (from
// the interim-hook goroutine) and the pong writes (from the read loop)
// must serialise through writeMu.
type serverConn struct {
	conn    *websocket.Conn
	peer    string
	writeMu sync.Mutex
}

func (sc *serverConn) write(ctx context.Context, env Envelope) error {
	sc.writeMu.Lock()
	defer sc.writeMu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return wsjson.Write(wctx, sc.conn, env)
}

// NewServer validates options and loads the TLS material.
func NewServer(opts ServerOptions) (*Server, error) {
	if opts.Log == nil {
		return nil, errors.New("sidechannel: server Log must not be nil")
	}
	if opts.ListenAddr == "" && opts.Listener == nil {
		return nil, errors.New("sidechannel: server needs ListenAddr or Listener")
	}
	tlsCfg, err := serverTLSConfig(opts.CACertPath, opts.ServerCert, opts.ServerKey)
	if err != nil {
		return nil, err
	}
	return &Server{
		opts:          opts,
		tlsConfig:     tlsCfg,
		log:           opts.Log,
		conns:         make(map[*serverConn]struct{}),
		activeStreams: make(map[string]struct{}),
	}, nil
}

// Run serves until ctx is cancelled (then it shuts down gracefully
// and returns nil) or the listener fails (then it returns the error).
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/sidechannel", s.handle)
	httpSrv := &http.Server{
		Handler:           mux,
		TLSConfig:         s.tlsConfig,
		ReadHeaderTimeout: 10 * time.Second,
		// Derive every request context from ctx so that cancelling ctx
		// also cancels in-flight handler reads. WebSocket connections
		// are hijacked, and neither Shutdown nor Close tears those
		// down; tying r.Context() to ctx is what lets a shutdown
		// actually close active side-channel connections (and lets a
		// dropped peer free its handler).
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	ln := s.opts.Listener
	if ln == nil {
		var err error
		ln, err = net.Listen("tcp", s.opts.ListenAddr)
		if err != nil {
			return fmt.Errorf("sidechannel: listen %s: %w", s.opts.ListenAddr, err)
		}
	}

	errc := make(chan error, 1)
	go func() {
		s.log.Info("sidechannel server listening", "addr", ln.Addr().String())
		// Certs live in TLSConfig, so the path args are empty.
		err := httpSrv.ServeTLS(ln, "", "")
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
		return nil
	case err := <-errc:
		return err
	}
}

// ConnCount returns how many WebSocket connections have been accepted
// so far. Used by tests to assert (re)connections.
func (s *Server) ConnCount() int64 { return s.connCount.Load() }

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		s.log.Warn("sidechannel accept failed", "err", err)
		return
	}
	defer conn.CloseNow()

	sc := &serverConn{conn: conn, peer: peerCN(r)}
	n := s.connCount.Add(1)
	s.addConn(sc)
	defer s.removeConn(sc)
	s.log.Info("sidechannel client connected", "peer", sc.peer, "remote", r.RemoteAddr, "conn", n)

	ctx := r.Context()
	for {
		var env Envelope
		if err := wsjson.Read(ctx, conn, &env); err != nil {
			s.log.Info("sidechannel client gone", "peer", sc.peer, "err", err)
			return
		}
		switch env.Type {
		case TypePing:
			s.log.Info("sidechannel ping received, sending pong", "peer", sc.peer)
			if err := sc.write(ctx, Envelope{Type: TypePong}); err != nil {
				s.log.Warn("sidechannel pong write failed", "peer", sc.peer, "err", err)
				return
			}
		case TypeStartPublish:
			s.markActive(env.StreamID)
			s.log.Info("sidechannel start_publish",
				"peer", sc.peer, "stream_id", env.StreamID, "has_token", env.PublishToken != "")
			if s.opts.OnStartPublish != nil {
				s.opts.OnStartPublish(env.StreamID, env.PublishToken)
			}
		case TypeStopPublish:
			s.markInactive(env.StreamID)
			s.log.Info("sidechannel stop_publish",
				"peer", sc.peer, "stream_id", env.StreamID, "reason", env.Reason)
			if s.opts.OnStopPublish != nil {
				s.opts.OnStopPublish(env.StreamID, env.Reason)
			}
		default:
			// Forward-compatible: a newer edge may send cargo types
			// this cloud build predates. Log and ignore, never crash.
			s.log.Warn("sidechannel unknown message type", "type", env.Type, "peer", sc.peer)
		}
	}
}

// RequestPublish sends a request_publish frame to every connected edge
// and returns how many it reached. One edge today; routing a stream_id
// to a specific edge is future work, so this broadcasts.
func (s *Server) RequestPublish(ctx context.Context, streamID string) int {
	s.mu.Lock()
	targets := make([]*serverConn, 0, len(s.conns))
	for sc := range s.conns {
		targets = append(targets, sc)
	}
	minter := s.iceMinter
	s.mu.Unlock()

	// Cloud-mint the short-lived TURN ICE servers for this request
	// (carvilon_stream sets the minter; nil -> host-only, no relay).
	var ice []streampublish.ICEServer
	if minter != nil {
		ice = minter(streamID)
	}

	sent := 0
	for _, sc := range targets {
		if err := sc.write(ctx, Envelope{Type: TypeRequestPublish, StreamID: streamID, ICEServers: ice}); err != nil {
			s.log.Warn("sidechannel request_publish write failed",
				"peer", sc.peer, "stream_id", streamID, "err", err)
			continue
		}
		sent++
	}
	s.log.Info("sidechannel request_publish sent", "stream_id", streamID, "edges", sent)
	return sent
}

// ActiveStreams returns a snapshot of the stream_ids the edge currently
// reports as publishing. In-memory only, no persistence.
func (s *Server) ActiveStreams() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.activeStreams))
	for id := range s.activeStreams {
		out = append(out, id)
	}
	return out
}

func (s *Server) addConn(sc *serverConn) {
	s.mu.Lock()
	s.conns[sc] = struct{}{}
	s.mu.Unlock()
}

func (s *Server) removeConn(sc *serverConn) {
	s.mu.Lock()
	delete(s.conns, sc)
	s.mu.Unlock()
}

func (s *Server) markActive(streamID string) {
	if streamID == "" {
		return
	}
	s.mu.Lock()
	s.activeStreams[streamID] = struct{}{}
	s.mu.Unlock()
}

func (s *Server) markInactive(streamID string) {
	s.mu.Lock()
	delete(s.activeStreams, streamID)
	s.mu.Unlock()
}

// peerCN returns the verified client certificate's CommonName, or ""
// if none was presented (which cannot happen once the handler runs,
// because RequireAndVerifyClientCert rejects certless handshakes
// before this point).
func peerCN(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	return r.TLS.PeerCertificates[0].Subject.CommonName
}

func serverTLSConfig(caPath, certPath, keyPath string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("sidechannel: load server keypair: %w", err)
	}
	pool, err := caPool(caPath)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// caPool reads a PEM CA bundle into a cert pool. Shared by both the
// server (ClientCAs) and the client (RootCAs).
func caPool(caPath string) (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("sidechannel: read ca cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("sidechannel: ca cert %s has no usable certificates", caPath)
	}
	return pool, nil
}
