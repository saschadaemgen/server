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
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
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
	return &Server{opts: opts, tlsConfig: tlsCfg, log: opts.Log}, nil
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

	peer := peerCN(r)
	n := s.connCount.Add(1)
	s.log.Info("sidechannel client connected", "peer", peer, "remote", r.RemoteAddr, "conn", n)

	ctx := r.Context()
	for {
		var env Envelope
		if err := wsjson.Read(ctx, conn, &env); err != nil {
			s.log.Info("sidechannel client gone", "peer", peer, "err", err)
			return
		}
		switch env.Type {
		case TypePing:
			s.log.Info("sidechannel ping received, sending pong", "peer", peer)
			if err := wsjson.Write(ctx, conn, Envelope{Type: TypePong}); err != nil {
				s.log.Warn("sidechannel pong write failed", "peer", peer, "err", err)
				return
			}
		default:
			// Forward-compatible: a newer edge may send cargo types
			// this cloud build predates. Log and ignore, never crash.
			s.log.Warn("sidechannel unknown message type", "type", env.Type, "peer", peer)
		}
	}
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
