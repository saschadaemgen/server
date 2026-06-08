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
	"sync"
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
	// EgressHMACKey is the egress-token HMAC key, already hex-decoded
	// (S3 egress-auth). It is SEPARATE from HMACKey: an egress token is
	// byte-identical to a publish token (same format/claims) but signed
	// with this key, so the same Verify validates it. When set, a WHEP
	// subscriber must present a valid Bearer egress token. When EMPTY, the
	// WHEP egress FAILS CLOSED - every subscribe is rejected 401 (the door
	// is locked by default, never accidentally open).
	EgressHMACKey []byte
	// ICEServers, when set, mints the ICE server list (TURN URLs + fresh
	// ephemeral creds) for each accepted peer. nil -> peers use no
	// ICEServers (host-candidate only, the pre-TURN behaviour). (S3 TURN)
	ICEServers func() ([]webrtc.ICEServer, error)
	// RequestPublish, when set, is the cold-start WHEP trigger: a subscriber
	// for a stream with no active publisher makes handleWHEP call this to
	// ask the edge to publish, then wait for the publisher to dock. It
	// returns the number of edges that received the request. nil -> 404 on a
	// missing publisher (the pre-trigger behaviour). (S3 WHEP trigger)
	RequestPublish func(ctx context.Context, streamID string) (edges int)

	// WHEPPublicAddr, when non-empty, enables a SECOND TLS listener on that
	// address serving ONLY the WHEP egress route (POST /whep/{streamID}) with
	// the public cert below. The primary Addr listener (WHIP + WHEP, private
	// cloudca) is left UNTOUCHED - it stays the edge publisher path. Empty ->
	// the public WHEP listener is OFF (opt-in; no break for existing
	// deployments). (S19-07 Baustufe 2)
	WHEPPublicAddr string
	// WHEPPublicCertFile / WHEPPublicKeyFile are the publicly-trusted cert/key
	// (e.g. Let's Encrypt) for the public WHEP listener, SEPARATE from the
	// cloudca CertFile/KeyFile. Required when WHEPPublicAddr is set.
	WHEPPublicCertFile string
	WHEPPublicKeyFile  string
}

const defaultAddr = ":8444"

// Server is the WHIP ingress TLS listener. Construct with [New], run
// with [Server.ListenAndServe].
type Server struct {
	addr      string
	certFile  string
	keyFile   string
	hmacKey   []byte
	egressKey []byte
	hub       *streamhub.Hub
	logger    *log.Logger
	srv       *http.Server

	// Public WHEP egress listener (S19-07 Baustufe 2). whepPublicSrv is nil
	// when WHEPPublicAddr was empty (feature off); when set it serves ONLY
	// POST /whep/{streamID} on whepPublicAddr with the public cert/key, while
	// the primary srv (WHIP + WHEP, cloudca) is untouched.
	whepPublicAddr     string
	whepPublicCertFile string
	whepPublicKeyFile  string
	whepPublicSrv      *http.Server

	// iceServers mints a fresh ICE server list per peer (TURN; S3). nil
	// -> no ICEServers (host-candidate only).
	iceServers func() ([]webrtc.ICEServer, error)

	// requestPublish is the cold-start WHEP trigger (S3). nil -> 404 on a
	// missing publisher.
	requestPublish func(ctx context.Context, streamID string) (edges int)
	// inflight is the per-streamID single-flight guard for the cold-start
	// trigger: at most one request_publish is in flight per stream while
	// simultaneous subscribers wait for the same publisher. A double
	// request_publish would be harmless (the edge publishes once), but this
	// avoids N frames for N simultaneous cold subscribers.
	inflightMu sync.Mutex
	inflight   map[string]struct{}
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
		addr:           cfg.Addr,
		certFile:       cfg.CertFile,
		keyFile:        cfg.KeyFile,
		hmacKey:        cfg.HMACKey,
		egressKey:      cfg.EgressHMACKey,
		hub:            cfg.Hub,
		logger:         logger,
		iceServers:     cfg.ICEServers,
		requestPublish: cfg.RequestPublish,
		inflight:       make(map[string]struct{}),
	}
	if len(s.egressKey) == 0 {
		// Fail closed: without an egress key the WHEP egress rejects every
		// subscribe (401). Loud one-shot WARN so a missing key is obvious.
		logger.Printf("whep egress: WARNING egress auth not configured (no key) -> ALL WHEP subscribes will be rejected 401 (fail closed)")
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

	// Optional public WHEP-egress listener (S19-07 Baustufe 2). When enabled
	// it serves ONLY the WHEP egress route with a publicly-trusted cert, on a
	// separate port; the primary listener above (WHIP + WHEP, cloudca) is
	// untouched. Require cert+key when the addr is set - no silent half-config.
	if cfg.WHEPPublicAddr != "" {
		if cfg.WHEPPublicCertFile == "" || cfg.WHEPPublicKeyFile == "" {
			return nil, errors.New("whip: WHEPPublicCertFile and WHEPPublicKeyFile are required when WHEPPublicAddr is set")
		}
		s.whepPublicAddr = cfg.WHEPPublicAddr
		s.whepPublicCertFile = cfg.WHEPPublicCertFile
		s.whepPublicKeyFile = cfg.WHEPPublicKeyFile
		// Same handler as the primary listener (same egress-token auth, same
		// hub, same cold-start trigger); only the cert + the route set differ.
		// No /whip route here -> a publish attempt on the public port 404s.
		publicMux := http.NewServeMux()
		publicMux.HandleFunc("POST /whep/{streamID}", s.handleWHEP)
		s.whepPublicSrv = &http.Server{
			Addr:              cfg.WHEPPublicAddr,
			Handler:           publicMux,
			ReadHeaderTimeout: 10 * time.Second,
		}
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

	// Optional public WHEP-egress listener (S19-07 Baustufe 2): a SEPARATE
	// port with a publicly-trusted cert serving ONLY POST /whep/{streamID}.
	// The primary ingress above (WHIP + WHEP, private cloudca) is untouched -
	// it stays the edge publisher path. nil when WHEPPublicAddr was empty.
	if s.whepPublicSrv != nil {
		pln, perr := net.Listen("tcp", s.whepPublicAddr)
		if perr != nil {
			_ = ln.Close()
			return fmt.Errorf("whip: public WHEP listen %s: %w", s.whepPublicAddr, perr)
		}
		s.logger.Printf("whip: public WHEP egress on %s (public cert)", pln.Addr())
		return s.serveBoth(ctx, ln, pln)
	}
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

// serveBoth runs the primary listener (WHIP + WHEP, cloudca) and the public
// WHEP-egress listener (WHEP only, public cert) concurrently, draining both
// on ctx cancel. A fatal error from either tears the other down and returns;
// the embedding runCloud then disables the stream subsystem while the
// side-channel keeps running. Mirrors serve()'s ctx/drain contract.
func (s *Server) serveBoth(ctx context.Context, mainLn, publicLn net.Listener) error {
	serveDone := make(chan error, 2)
	go func() { serveDone <- s.srv.ServeTLS(mainLn, s.certFile, s.keyFile) }()
	go func() {
		serveDone <- s.whepPublicSrv.ServeTLS(publicLn, s.whepPublicCertFile, s.whepPublicKeyFile)
	}()

	shutdownBoth := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = s.srv.Shutdown(shutdownCtx)
		_ = s.whepPublicSrv.Shutdown(shutdownCtx)
	}

	select {
	case <-ctx.Done():
		shutdownBoth()
		return ctx.Err()
	case err := <-serveDone:
		// One listener stopped; tear the other down too, then report. The
		// other goroutine's ErrServerClosed lands in the buffered channel and
		// is discarded.
		shutdownBoth()
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
// subscriber POSTs an SDP offer and receives the publisher's fan-out track.
// It verifies a Bearer egress token first (S3 egress-auth, a SEPARATE key),
// mirroring the ingress: the concrete failure is logged, the client sees
// only a bare 401. The auth check runs BEFORE the cold-start trigger, so an
// unauthorized subscriber can never force the edge to publish.
func (s *Server) handleWHEP(w http.ResponseWriter, r *http.Request) {
	streamID := r.PathValue("streamID")
	if streamID == "" {
		http.Error(w, "missing streamID", http.StatusBadRequest)
		return
	}

	// Egress auth (S3): verify the Bearer egress token BEFORE anything else
	// - in particular BEFORE the cold-start trigger below - so an
	// unauthorized subscriber can never force the edge to publish. The
	// egress token is byte-identical to a publish token but signed with a
	// SEPARATE key, so the same Verify validates it. Fail closed: with no
	// egress key configured, reject every subscribe.
	if len(s.egressKey) == 0 {
		s.logger.Printf("whep egress: sid=%s rejected: egress auth not configured (no key)", streamID)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	const bearerPrefix = "Bearer "
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, bearerPrefix) {
		s.logger.Printf("whep egress: sid=%s rejected: missing bearer", streamID)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := publishtoken.Verify(strings.TrimPrefix(authz, bearerPrefix), streamID, s.egressKey, time.Now().UTC()); err != nil {
		// Concrete failure class (ErrMalformed/ErrSignature/ErrSIDMismatch/
		// ErrExpired) logged only; the client gets a bare 401, no oracle.
		s.logger.Printf("whep egress: sid=%s rejected: %v", streamID, err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

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
	// Cold-start trigger (S3): no publisher yet AND the Master wired the
	// soft-gated RequestPublish callback -> ask the edge to publish, wait
	// for it to dock, then attach. nil callback -> unchanged (404 below).
	if errors.Is(err, ErrNoPublisher) && s.requestPublish != nil {
		sdpAnswer, sessionID, err = s.coldSubscribe(r.Context(), streamID, string(body), iceServers)
	}
	if err != nil {
		switch {
		case errors.Is(err, ErrNoPublisher):
			s.logger.Printf("whep egress: sid=%s no active publisher", streamID)
			http.Error(w, "no active publisher for stream", http.StatusNotFound)
		case errors.Is(err, ErrTrackNotReady):
			s.logger.Printf("whep egress: sid=%s track not ready", streamID)
			http.Error(w, "stream starting, retry", http.StatusServiceUnavailable)
		case errors.Is(err, errNoEdge), errors.Is(err, errColdPublishTimeout):
			// The trigger ran (an edge was asked), so this is NOT a 404:
			// the publisher could not be brought up in time / no edge took it.
			s.logger.Printf("whep egress: sid=%s cold publish failed: %v", streamID, err)
			http.Error(w, "stream could not be started", http.StatusGatewayTimeout)
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

// coldSubscribe handles the cold-start path: no publisher exists yet, so it
// asks the edge to publish (via the soft-gated RequestPublish callback) and
// waits up to coldPublishTimeout for the publisher to dock in the hub, then
// attaches the subscriber normally. Single-flight per streamID: simultaneous
// cold subscribers trigger at most one request_publish; the followers just
// wait for the same publisher. Caller guarantees s.requestPublish != nil.
func (s *Server) coldSubscribe(ctx context.Context, streamID, sdpOffer string, iceServers []webrtc.ICEServer) (string, string, error) {
	lead, done := s.beginTrigger(streamID)
	if lead {
		defer done()
		edges := s.requestPublish(ctx, streamID)
		s.logger.Printf("whep egress: sid=%s cold subscribe -> request_publish (edges=%d)", streamID, edges)
		if edges < 1 {
			// No edge received the request; the stream cannot start. Fail
			// fast WITHOUT waiting.
			return "", "", errNoEdge
		}
	} else {
		s.logger.Printf("whep egress: sid=%s cold subscribe -> request_publish already in flight, waiting", streamID)
	}

	// Wait for the publisher to dock (request_publish -> edge -> POST /whip
	// -> hub session). AcceptSubscriber's own WaitTrack then covers the
	// session->track gap.
	waitCtx, cancel := context.WithTimeout(ctx, coldPublishTimeout)
	defer cancel()
	if !waitForPublisherSession(waitCtx, s.hub, streamID) {
		return "", "", errColdPublishTimeout
	}
	return AcceptSubscriber(ctx, s.hub, s.logger, streamID, sdpOffer, iceServers)
}

// beginTrigger returns lead=true to exactly one caller per streamID while a
// request_publish is in flight; concurrent callers get lead=false and should
// just wait for the publisher (the leader already triggered). done clears the
// in-flight mark and must be called by the leader on completion.
func (s *Server) beginTrigger(streamID string) (lead bool, done func()) {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if _, busy := s.inflight[streamID]; busy {
		return false, func() {}
	}
	s.inflight[streamID] = struct{}{}
	return true, func() {
		s.inflightMu.Lock()
		delete(s.inflight, streamID)
		s.inflightMu.Unlock()
	}
}

// isSDP reports whether the Content-Type names application/sdp,
// tolerating parameters (e.g. "application/sdp; charset=utf-8").
func isSDP(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == "application/sdp"
}
