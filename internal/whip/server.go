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

	"github.com/pion/webrtc/v4"

	"carvilon.local/stream/internal/publishtoken"
	"carvilon.local/stream/internal/streamhub"
)

// maxSDPBytes caps the request body. A WHIP SDP offer is a few KB; 64
// KiB is a generous ceiling that still bounds a hostile client.
const maxSDPBytes = 64 * 1024

// shutdownTimeout bounds the graceful-shutdown drain on ctx cancel.
const shutdownTimeout = 5 * time.Second

// Config configures a [Server].
type Config struct {
	Addr     string         // listen address, e.g. ":8444" (default if empty)
	CertFile string         // absolute path to the server certificate (PEM)
	KeyFile  string         // absolute path to the server private key (PEM)
	HMACKey  []byte         // publish-token HMAC key, already hex-decoded
	Hub      *streamhub.Hub // active-publisher registry (S2-04)
	Logger   *log.Logger
	// ICEServers, when set, mints the ICE server list (TURN URLs + fresh
	// ephemeral creds) for each accepted peer. nil -> peers use no
	// ICEServers (host-candidate only, the pre-TURN behaviour). (S3 TURN)
	ICEServers func() ([]webrtc.ICEServer, error)
}

const defaultAddr = ":8444"

// Server is the WHIP ingress TLS listener. Construct with [New], run
// with [Server.ListenAndServe].
type Server struct {
	addr     string
	certFile string
	keyFile  string
	hmacKey  []byte
	hub      *streamhub.Hub
	logger   *log.Logger
	srv      *http.Server

	// iceServers mints a fresh ICE server list per peer (TURN; S3). nil
	// -> no ICEServers (host-candidate only).
	iceServers func() ([]webrtc.ICEServer, error)
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
	if cfg.Hub == nil {
		return nil, errors.New("whip: Hub is required")
	}
	if cfg.Addr == "" {
		cfg.Addr = defaultAddr
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}

	s := &Server{
		addr:       cfg.Addr,
		certFile:   cfg.CertFile,
		keyFile:    cfg.KeyFile,
		hmacKey:    cfg.HMACKey,
		hub:        cfg.Hub,
		logger:     logger,
		iceServers: cfg.ICEServers,
	}

	mux := http.NewServeMux()
	// Go 1.22 method+path pattern: only POST matches; GET/PUT/DELETE/
	// PATCH on the same path yield 405 automatically. A request to
	// "/whip/" (empty {streamID}) does not match and yields 404.
	mux.HandleFunc("POST /whip/{streamID}", s.handlePublish)
	// WHEP egress (S2-05): subscribers POST an SDP offer, receive the
	// fan-out track. Same path-pattern semantics as the ingress.
	mux.HandleFunc("POST /whep/{streamID}", s.handleWHEP)

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

// mintICEServers returns the ICE server list for a freshly accepted peer
// (TURN URLs + fresh ephemeral creds via the configured minter), or nil
// when TURN is off (host-candidate only). (S3)
func (s *Server) mintICEServers() ([]webrtc.ICEServer, error) {
	if s.iceServers == nil {
		return nil, nil
	}
	return s.iceServers()
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

	// 5. Token verified, SDP in hand: build the PeerConnection, accept
	// the publisher's RTP into the hub, and answer per WHIP (RFC 9725):
	// 201 Created, SDP answer in the body, Location header pointing at
	// the (future) session resource.
	iceServers, err := s.mintICEServers()
	if err != nil {
		s.logger.Printf("whip ingress: sid=%s ICE servers: %v", streamID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	sdpAnswer, sessionID, err := AcceptPublisher(r.Context(), s.hub, s.logger, streamID, string(body), iceServers)
	if err != nil {
		if errors.Is(err, streamhub.ErrConflict) {
			s.logger.Printf("whip ingress: conflict for sid=%s (already publishing)", streamID)
			http.Error(w, "stream already publishing", http.StatusConflict)
			return
		}
		s.logger.Printf("whip ingress: accept publisher failed for sid=%s: %v", streamID, err)
		http.Error(w, "publisher setup failed", http.StatusInternalServerError)
		return
	}

	location := fmt.Sprintf("/whip/%s/session/%s", streamID, sessionID)
	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", location)
	w.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(w, sdpAnswer)
	s.logger.Printf("whip ingress: accepted sid=%s session=%s, sdp bytes=%d", streamID, sessionID, len(body))
}

// handleWHEP implements POST /whep/{streamID} — the egress side. A
// subscriber POSTs an SDP offer and receives the publisher's fan-out
// track. Unlike the ingress, S2-05 does NOT verify a bearer token here:
// the egress-auth scheme is deferred (Master Option 3), and
// AcceptSubscriber carries the marked pass-through hook. Once that hook
// gates, this handler grows a 401 branch.
func (s *Server) handleWHEP(w http.ResponseWriter, r *http.Request) {
	streamID := r.PathValue("streamID")
	if streamID == "" {
		http.Error(w, "missing streamID", http.StatusBadRequest)
		return
	}

	// TODO egress-auth: no token check in S2-05 (Master Option 3). When
	// the egress-token spec lands, validate here BEFORE reading the body
	// and surface a bare 401 on failure, mirroring the ingress.

	if !isSDP(r.Header.Get("Content-Type")) {
		s.logger.Printf("whep egress: sid=%s rejected: content-type %q (want application/sdp)",
			streamID, r.Header.Get("Content-Type"))
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxSDPBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.logger.Printf("whep egress: sid=%s read body: %v", streamID, err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	iceServers, err := s.mintICEServers()
	if err != nil {
		s.logger.Printf("whep egress: sid=%s ICE servers: %v", streamID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	sdpAnswer, sessionID, err := AcceptSubscriber(r.Context(), s.hub, s.logger, streamID, string(body), iceServers)
	if err != nil {
		switch {
		case errors.Is(err, ErrNoPublisher):
			s.logger.Printf("whep egress: sid=%s no active publisher", streamID)
			http.Error(w, "no active publisher for stream", http.StatusNotFound)
		case errors.Is(err, ErrTrackNotReady):
			s.logger.Printf("whep egress: sid=%s track not ready", streamID)
			http.Error(w, "stream starting, retry", http.StatusServiceUnavailable)
		default:
			s.logger.Printf("whep egress: sid=%s subscriber setup failed: %v", streamID, err)
			http.Error(w, "subscriber setup failed", http.StatusInternalServerError)
		}
		return
	}

	location := fmt.Sprintf("/whep/%s/session/%s", streamID, sessionID)
	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", location)
	w.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(w, sdpAnswer)
	s.logger.Printf("whep egress: accepted sid=%s session=%s, offer bytes=%d", streamID, sessionID, len(body))
}

// isSDP reports whether the Content-Type names application/sdp,
// tolerating parameters (e.g. "application/sdp; charset=utf-8").
func isSDP(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == "application/sdp"
}
