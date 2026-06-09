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
	"carvilon.local/server/internal/streamstore"
	"carvilon.local/server/internal/turnstore"
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
	// cloud attaches to each request_publish frame AND answers request_ice
	// with. Set by the carvilon_stream-tagged cloud closure (which holds the
	// TURN shared secret); nil in the public build -> request_publish carries
	// no ICE and request_ice replies empty (host-only, the pre-TURN behaviour).
	iceMinter func(streamID string) []streampublish.ICEServer
	// iceCredTTLSeconds is the lifetime of the minted TURN credentials,
	// reported back to the edge on ice_servers. Set together with iceMinter
	// (the closure derives it from the mint TTL); 0 until set.
	iceCredTTLSeconds int
	// whepBaseURL is the public WHEP base URL the cloud advertises on every
	// ice_servers reply (Saison 19-08). Set once at setup by the
	// carvilon_stream cloud closure from CloudServer.WHEPPublicBaseURL();
	// empty when the public WHEP listener is off -> the edge uses its interim
	// base.
	whepBaseURL string
	// bundlePending correlates an in-flight RequestBundle (by request_id) with
	// the bundle_reply the read loop delivers - the cloud-side mirror of the
	// edge's request_ice pending-map. Guarded by mu. (Saison 19-11)
	bundlePending map[string]chan bundleReply
	// httpPending correlates an in-flight RelayHTTP (by request_id) with the
	// http_reply the read loop delivers - the generic control-relay's
	// pending-map, same pattern as bundlePending. Guarded by mu. (Saison 19-27)
	httpPending map[string]chan httpReply
}

// SetICEMinter installs the per-request TURN ICE-server minter and the TTL
// (seconds) of the credentials it mints. The carvilon_stream-tagged cloud
// closure calls this with a closure that mints short-lived credentials from
// the TURN shared secret plus the derived credential TTL; the public build
// never calls it. Set once before Run; guarded by the same mutex
// RequestPublish and the request_ice handler read under.
func (s *Server) SetICEMinter(m func(streamID string) []streampublish.ICEServer, credTTLSeconds int) {
	s.mu.Lock()
	s.iceMinter = m
	s.iceCredTTLSeconds = credTTLSeconds
	s.mu.Unlock()
}

// SetWHEPBaseURL installs the public WHEP base URL the cloud advertises on
// every ice_servers reply (Saison 19-08). The carvilon_stream cloud closure
// calls it once at setup with CloudServer.WHEPPublicBaseURL() (empty when the
// public WHEP listener is off). Guarded by the same mutex replyICE reads under.
func (s *Server) SetWHEPBaseURL(base string) {
	s.mu.Lock()
	s.whepBaseURL = base
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
		bundlePending: make(map[string]chan bundleReply),
		httpPending:   make(map[string]chan httpReply),
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
		case TypeRequestICE:
			// Mint a fresh ICE set and reply to GENAU this connection. A
			// write failure drops the conn (same as the pong path); the
			// edge's RequestICE then times out and the bundle 503s.
			if err := s.replyICE(ctx, sc, env); err != nil {
				s.log.Warn("sidechannel ice_servers write failed",
					"peer", sc.peer, "request_id", env.RequestID, "err", err)
				return
			}
		case TypeBundleReply:
			// Edge's answer to a cloud RequestBundle; route it to the waiting
			// caller by RequestID (mirror of the edge's ice_servers delivery).
			s.deliverBundleReply(env)
		case TypeHTTPReply:
			// Edge's answer to a cloud RelayHTTP; route it by RequestID.
			s.deliverHTTPReply(env)
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

// replyICE answers an edge's request_ice: mint a fresh ICE set with the SAME
// minter RequestPublish uses (the creds are sid-agnostic, so StreamID is only
// passed through for the closure's own logging) and send an ice_servers frame
// back to GENAU the requesting connection sc - never a broadcast - mirroring
// the RequestID and reporting the credential TTL. With no minter set (public
// build / TURN off) it replies with an empty ICE list, which the edge treats
// as a failure, plus a WARN. Returns the write error so the read loop can drop
// the conn on a failed write, matching the pong path.
func (s *Server) replyICE(ctx context.Context, sc *serverConn, env Envelope) error {
	s.mu.Lock()
	minter := s.iceMinter
	ttl := s.iceCredTTLSeconds
	whepBase := s.whepBaseURL
	s.mu.Unlock()

	var ice []streampublish.ICEServer
	if minter != nil {
		ice = minter(env.StreamID)
	} else {
		s.log.Warn("sidechannel request_ice but no ICE minter set; replying empty",
			"peer", sc.peer, "request_id", env.RequestID)
	}
	s.log.Info("sidechannel request_ice answered",
		"peer", sc.peer, "request_id", env.RequestID,
		"stream_id", env.StreamID, "ice_servers", len(ice), "whep_base", whepBase != "")
	return sc.write(ctx, Envelope{
		Type:             TypeICEServers,
		RequestID:        env.RequestID,
		ICEServers:       ice,
		ExpiresInSeconds: ttl,
		WHEPBaseURL:      whepBase,
	})
}

// bundleRequestTimeout bounds a RequestBundle round-trip when the caller's ctx
// carries no deadline of its own. The edge work (resolve + egress mint) is
// sub-second; this is just a backstop.
const bundleRequestTimeout = 5 * time.Second

// ErrNoEdge is returned by RequestBundle when no edge is connected to relay
// the bundle request to. The caller maps it (and any timeout) to 503.
var ErrNoEdge = errors.New("sidechannel: no edge connected")

// ErrBundleAuth is returned by RequestBundle when the edge rejected the
// request (auth failed / could not mint). The caller maps it to a bare 401.
// The concrete reason stays in the edge log; it never crosses to the client.
var ErrBundleAuth = errors.New("sidechannel: bundle rejected by edge")

// bundleReply is the edge's bundle_reply routed back to the waiting
// RequestBundle call by RequestID.
type bundleReply struct {
	mac         string
	egressToken string
	expiresIn   int
	edgeWHEPURL string // Saison 19-41: LAN-direct WHEP URL from the edge (may be empty)
	errMsg      string
}

// RequestBundle asks the edge - over the side-channel - to resolve a remote
// subscriber's Bearer to a viewer MAC and mint a sid-bound egress token. It is
// the cloud-side mirror of the edge's RequestICE: a request_id correlates the
// bundle_reply back to this call. It does NOT assemble the bundle (the cloud
// adds ICE + WHEP URL via AssembleBundle); it returns only the two edge-only
// parts. ErrBundleAuth -> the edge rejected (caller maps to 401); ErrNoEdge /
// ctx.Err() -> no edge answered (caller maps to 503). The RPC is fast (no
// publisher wait); the cold-start happens later, on the WHEP POST.
func (s *Server) RequestBundle(ctx context.Context, credential string) (mac, egressToken string, expiresIn int, edgeWHEPURL string, err error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, bundleRequestTimeout)
		defer cancel()
	}
	id, gerr := newRequestID()
	if gerr != nil {
		return "", "", 0, "", fmt.Errorf("sidechannel: request id: %w", gerr)
	}

	// Pick one edge conn (one edge today; routing a viewer to a specific edge
	// is future work, same caveat as RequestPublish). Buffered cap-1 reply
	// chan + delete-on-return so a timeout never leaks a pending entry.
	ch := make(chan bundleReply, 1)
	s.mu.Lock()
	var target *serverConn
	for sc := range s.conns {
		target = sc
		break
	}
	s.bundlePending[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.bundlePending, id)
		s.mu.Unlock()
	}()

	if target == nil {
		return "", "", 0, "", ErrNoEdge
	}
	if werr := target.write(ctx, Envelope{Type: TypeBundleRequest, RequestID: id, Credential: credential}); werr != nil {
		s.log.Warn("sidechannel bundle_request write failed", "peer", target.peer, "request_id", id, "err", werr)
		return "", "", 0, "", ErrNoEdge
	}

	select {
	case reply := <-ch:
		if reply.errMsg != "" {
			return "", "", 0, "", ErrBundleAuth
		}
		return reply.mac, reply.egressToken, reply.expiresIn, reply.edgeWHEPURL, nil
	case <-ctx.Done():
		return "", "", 0, "", ctx.Err()
	}
}

// deliverBundleReply routes a bundle_reply frame to the waiting RequestBundle
// call by RequestID (cloud-side mirror of the edge's deliverICEReply). A
// missing entry means the caller already timed out -> log and drop. The send
// is non-blocking (cap-1 chan), so the read loop never stalls.
func (s *Server) deliverBundleReply(env Envelope) {
	s.mu.Lock()
	ch, ok := s.bundlePending[env.RequestID]
	s.mu.Unlock()
	if !ok {
		s.log.Warn("sidechannel bundle_reply for unknown/expired request, dropping", "request_id", env.RequestID)
		return
	}
	select {
	case ch <- bundleReply{mac: env.MAC, egressToken: env.EgressToken, expiresIn: env.ExpiresInSeconds, edgeWHEPURL: env.EdgeWHEPURL, errMsg: env.Error}:
	default:
		s.log.Warn("sidechannel duplicate bundle_reply, dropping", "request_id", env.RequestID)
	}
}

// AssembleBundle builds the full stream-start bundle from the edge-provided
// parts (mac + egress token + TTL) plus the cloud-held parts: the ICE servers
// (minted here - the cloud holds the TURN secret) and the public WHEP URL
// (base + /whep/<mac>). It is the SINGLE cloud-side assembler. ICEServers is
// empty when no minter is set; WHEPURL is empty when no public WHEP base is
// set - the caller then declines (a bundle without a WHEP URL is not
// actionable for the subscriber).
func (s *Server) AssembleBundle(mac, egressToken string, expiresIn int, edgeWHEPURL string) streampublish.StreamStartBundle {
	s.mu.Lock()
	minter := s.iceMinter
	base := s.whepBaseURL
	s.mu.Unlock()

	var ice []streampublish.ICEServer
	if minter != nil {
		ice = minter(mac)
	}
	whepURL := ""
	if base != "" {
		whepURL = base + "/whep/" + mac
	}
	return streampublish.StreamStartBundle{
		WHEPURL:     whepURL,
		EgressToken: egressToken,
		StreamID:    mac,
		ICEServers:  ice,
		ExpiresIn:   expiresIn,
		EdgeWHEPURL: edgeWHEPURL, // Saison 19-41: edge-supplied LAN-direct URL (omitempty)
	}
}

// relayRequestTimeout bounds a RelayHTTP round-trip when the caller's ctx
// carries no deadline. More generous than bundleRequestTimeout: the relayed
// handler may itself call the UA-API (e.g. door unlock).
const relayRequestTimeout = 10 * time.Second

// maxRelayReplyBody defensively caps the response body framed back from the
// edge (the control responses are tiny JSON).
const maxRelayReplyBody = 64 * 1024

// ErrRelayFailed is returned by RelayHTTP when the edge could not RUN the
// request at all (mechanism failure). The caller maps it (and ErrNoEdge /
// timeout) to a cloud-generated 503. A NORMAL HTTP status (incl. 401/404) is
// NOT an error - it rides RelayResponse.Status.
var ErrRelayFailed = errors.New("sidechannel: edge could not run relayed request")

// RelayRequest is the curated HTTP request the cloud relays to the edge. Path
// is the FULL path (incl. path params). Header carries only Authorization +
// Content-Type. Body is capped by the caller.
type RelayRequest struct {
	Method   string
	Path     string
	RawQuery string
	Header   map[string]string
	Body     []byte
}

// RelayResponse is the edge's captured HTTP response. Header carries only
// Content-Type.
type RelayResponse struct {
	Status int
	Header map[string]string
	Body   []byte
}

// httpReply is the edge's http_reply routed back to the waiting RelayHTTP call
// by RequestID.
type httpReply struct {
	status int
	header map[string]string
	body   []byte
	errMsg string
}

// RelayHTTP forwards a remote app's control call to the edge over the
// side-channel and returns the edge's captured HTTP response. It is the
// generalisation of RequestBundle: same request_id correlation, pending-map and
// one-edge-today targeting. The relay is a DUMB PIPE - the edge runs the
// request through its own mux (requireViewerAuth + the unchanged handler) and
// IS the auth authority; RelayHTTP never inspects the credential. A normal HTTP
// status (incl. 401) comes back in RelayResponse.Status; ErrNoEdge / ctx.Err()
// / ErrRelayFailed mean the relay itself could not complete (caller -> 503).
func (s *Server) RelayHTTP(ctx context.Context, req RelayRequest) (RelayResponse, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, relayRequestTimeout)
		defer cancel()
	}
	id, gerr := newRequestID()
	if gerr != nil {
		return RelayResponse{}, fmt.Errorf("sidechannel: request id: %w", gerr)
	}

	ch := make(chan httpReply, 1)
	s.mu.Lock()
	var target *serverConn
	for sc := range s.conns {
		target = sc
		break
	}
	s.httpPending[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.httpPending, id)
		s.mu.Unlock()
	}()

	if target == nil {
		return RelayResponse{}, ErrNoEdge
	}
	if werr := target.write(ctx, Envelope{
		Type:       TypeHTTPRequest,
		RequestID:  id,
		Method:     req.Method,
		Path:       req.Path,
		RawQuery:   req.RawQuery,
		HTTPHeader: req.Header,
		Body:       req.Body,
	}); werr != nil {
		s.log.Warn("sidechannel http_request write failed", "peer", target.peer, "request_id", id, "err", werr)
		return RelayResponse{}, ErrNoEdge
	}

	select {
	case reply := <-ch:
		if reply.errMsg != "" {
			return RelayResponse{}, ErrRelayFailed
		}
		return RelayResponse{Status: reply.status, Header: reply.header, Body: reply.body}, nil
	case <-ctx.Done():
		return RelayResponse{}, ctx.Err()
	}
}

// deliverHTTPReply routes an http_reply frame to the waiting RelayHTTP call by
// RequestID (mirror of deliverBundleReply). The body is capped defensively.
func (s *Server) deliverHTTPReply(env Envelope) {
	s.mu.Lock()
	ch, ok := s.httpPending[env.RequestID]
	s.mu.Unlock()
	if !ok {
		s.log.Warn("sidechannel http_reply for unknown/expired request, dropping", "request_id", env.RequestID)
		return
	}
	body := env.Body
	if len(body) > maxRelayReplyBody {
		body = body[:maxRelayReplyBody]
	}
	select {
	case ch <- httpReply{status: env.Status, header: env.HTTPHeader, body: body, errMsg: env.Error}:
	default:
		s.log.Warn("sidechannel duplicate http_reply, dropping", "request_id", env.RequestID)
	}
}

// SendTURNEvent broadcasts one TURN telemetry event to every connected
// edge (cloud -> edge) and returns how many it reached. The relay lives
// on the cloud but the admin UI + SQLite live on the edge, so telemetry
// flows the same direction as request_publish. Safe to call
// concurrently; per-conn writes serialise through writeMu. The caller
// (cloud closure) funnels pion's concurrent events through a single
// buffered drain goroutine so a slow write never backs up the relay.
func (s *Server) SendTURNEvent(ctx context.Context, ev turnstore.Event) int {
	return s.broadcast(ctx, Envelope{Type: TypeTURNEvent, TURNEvent: &ev})
}

// SendTURNStats broadcasts a live relay snapshot to every connected
// edge (cloud -> edge), driven by the cloud's periodic ticker.
func (s *Server) SendTURNStats(ctx context.Context, snap turnstore.Snapshot) int {
	return s.broadcast(ctx, Envelope{Type: TypeTURNStats, TURNStats: &snap})
}

// SendStreamStats broadcasts a live cloud-viewer snapshot (per-stream WHEP
// consumer counts) to every connected edge (cloud -> edge), driven by the
// cloud's periodic ticker next to SendTURNStats. (S20)
func (s *Server) SendStreamStats(ctx context.Context, snap streamstore.Snapshot) int {
	return s.broadcast(ctx, Envelope{Type: TypeStreamStats, StreamStats: &snap})
}

// broadcast writes env to every connected edge and returns how many it
// reached. Per-conn writes serialise through writeMu (coder/websocket
// forbids concurrent writers). One edge today; this broadcasts.
func (s *Server) broadcast(ctx context.Context, env Envelope) int {
	s.mu.Lock()
	targets := make([]*serverConn, 0, len(s.conns))
	for sc := range s.conns {
		targets = append(targets, sc)
	}
	s.mu.Unlock()
	sent := 0
	for _, sc := range targets {
		if err := sc.write(ctx, env); err != nil {
			s.log.Warn("sidechannel broadcast write failed", "type", env.Type, "peer", sc.peer, "err", err)
			continue
		}
		sent++
	}
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
