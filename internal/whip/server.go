// Package whip implements the WHIP ingress endpoint for the cloud role.
//
// Spec: RFC 9725 (WHIP). This implementation handles only POST
// /whip/{streamID} for now; PATCH (trickle ICE) and DELETE (session
// teardown) are deferred to later seasons.
//
// S2-03 scope: TLS listener + Bearer-token verification. On a verified
// token the handler returns 501 Not Implemented — WebRTC track
// acceptance and fan-out land in S2-04. On any auth failure the client
// gets a bare 401 (no detail); the concrete reason is logged
// server-side only, so the endpoint is not a verification oracle.
package whip

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"

	"carvilon.local/stream/internal/publishtoken"
)

// maxSDPBytes caps the request body. A WHIP SDP offer is a few KB; 64
// KiB is a generous ceiling that still bounds a hostile client.
const maxSDPBytes = 64 * 1024

// shutdownTimeout bounds the graceful-shutdown drain on ctx cancel.
const shutdownTimeout = 5 * time.Second

// Config configures a [Server].
type Config struct {
	Addr     string // listen address, e.g. ":8444" (default if empty)
	CertFile string // absolute path to the server certificate (PEM)
	KeyFile  string // absolute path to the server private key (PEM)
	HMACKey  []byte // publish-token HMAC key, already hex-decoded
	Logger   *log.Logger
}

const defaultAddr = ":8444"

// Server is the WHIP ingress TLS listener. Construct with [New], run
// with [Server.ListenAndServe].
type Server struct {
	addr     string
	certFile string
	keyFile  string
	hmacKey  []byte
	logger   *log.Logger
	srv      *http.Server
}

// New validates the config and builds the (not-yet-listening) server.
// Cert/key file existence is checked lazily at serve time by the TLS
// stack; New only rejects an obviously incomplete config.
func New(cfg Config) (*Server, error) {
	if cfg.CertFile == "" || cfg.KeyFile == "" {
		return nil, errors.New("whip: CertFile and KeyFile are required")
	}
	if len(cfg.HMACKey) == 0 {
		return nil, errors.New("whip: HMACKey must not be empty")
	}
	if cfg.Addr == "" {
		cfg.Addr = defaultAddr
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}

	s := &Server{
		addr:     cfg.Addr,
		certFile: cfg.CertFile,
		keyFile:  cfg.KeyFile,
		hmacKey:  cfg.HMACKey,
		logger:   logger,
	}

	mux := http.NewServeMux()
	// Go 1.22 method+path pattern: only POST matches; GET/PUT/DELETE/
	// PATCH on the same path yield 405 automatically. A request to
	// "/whip/" (empty {streamID}) does not match and yields 404.
	mux.HandleFunc("POST /whip/{streamID}", s.handlePublish)

	s.srv = &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s, nil
}

// ListenAndServe binds the configured address and serves TLS until ctx
// is cancelled, then drains gracefully. Returns nil on a clean
// shutdown. Mirrors the edge server's Run pattern (net.Listen +
// ServeTLS) so a dynamic ":0" address is also usable (tests).
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("whip: listen %s: %w", s.addr, err)
	}
	s.logger.Printf("whip: TLS ingress on %s", ln.Addr())
	return s.serve(ctx, ln)
}

// serve runs the TLS accept loop on an already-bound listener. Split
// out from ListenAndServe so tests can hand in a 127.0.0.1:0 listener
// and learn the chosen port from ln.Addr().
func (s *Server) serve(ctx context.Context, ln net.Listener) error {
	serveDone := make(chan error, 1)
	go func() { serveDone <- s.srv.ServeTLS(ln, s.certFile, s.keyFile) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = s.srv.Shutdown(shutdownCtx)
		return ctx.Err()
	case err := <-serveDone:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// handlePublish implements POST /whip/{streamID}. See the package doc
// for the auth-vs-501 contract. Check order is security-relevant:
// auth before content-type before body, so an unauthenticated client
// learns nothing about the request shape.
func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	streamID := r.PathValue("streamID")
	if streamID == "" {
		http.Error(w, "missing streamID", http.StatusBadRequest)
		return
	}

	// 1. Bearer presence + prefix.
	const bearerPrefix = "Bearer "
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, bearerPrefix) {
		s.logger.Printf("whip ingress: sid=%s rejected: missing bearer", streamID)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	token := strings.TrimPrefix(authz, bearerPrefix)

	// 2. Token verification. The concrete failure class is logged but
	// NEVER surfaced — client sees a bare 401.
	if err := publishtoken.Verify(token, streamID, s.hmacKey, time.Now().UTC()); err != nil {
		s.logger.Printf("whip ingress: sid=%s rejected: %v", streamID, err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// 3. Content negotiation: WHIP carries an SDP offer.
	if !isSDP(r.Header.Get("Content-Type")) {
		s.logger.Printf("whip ingress: sid=%s rejected: content-type %q (want application/sdp)",
			streamID, r.Header.Get("Content-Type"))
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}

	// 4. Read the SDP offer under a hard byte cap.
	r.Body = http.MaxBytesReader(w, r.Body, maxSDPBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.logger.Printf("whip ingress: sid=%s read body: %v", streamID, err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// 5. Success path — track acceptance is S2-04. Token is verified,
	// SDP is in hand; we simply don't have the PeerConnection plumbing
	// yet. 501 is the honest status.
	s.logger.Printf("whip ingress: token verified for sid=%s, sdp bytes=%d, 501 (track acceptance pending)",
		streamID, len(body))
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusNotImplemented)
	_, _ = io.WriteString(w, "WHIP track acceptance pending S2-04")
}

// isSDP reports whether the Content-Type names application/sdp,
// tolerating parameters (e.g. "application/sdp; charset=utf-8").
func isSDP(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == "application/sdp"
}
