package sidechannel

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"carvilon.local/server/internal/streampublish"
	"carvilon.local/server/internal/streamstore"
	"carvilon.local/server/internal/turnstore"
)

const (
	defaultInitialBackoff = time.Second
	maxBackoff            = 30 * time.Second
	defaultPingInterval   = 15 * time.Second
	dialTimeout           = 10 * time.Second
	writeTimeout          = 10 * time.Second
	sendQueueSize         = 16
	// iceRequestTimeout bounds a RequestICE round-trip when the caller's
	// ctx carries no deadline of its own.
	iceRequestTimeout = 5 * time.Second
	// relayServeTimeout bounds the edge-side execution of a relayed control
	// call (Saison 19-27). Generous (the handler may call the UA-API);
	// matches the cloud's relayRequestTimeout so neither side waits much
	// longer than the other.
	relayServeTimeout = 10 * time.Second
)

// ErrNoICEServers is returned by RequestICE when the cloud answered with an
// empty ICE set (no minter configured cloud-side, or the mint failed).
var ErrNoICEServers = errors.New("sidechannel: cloud returned no ICE servers")

// iceReply is the cloud's ice_servers answer routed back to the waiting
// RequestICE call by RequestID.
type iceReply struct {
	servers     []streampublish.ICEServer
	expiresIn   int
	whepBaseURL string
}

// ClientOptions configures the edge-side dialer.
type ClientOptions struct {
	// URL is the cloud endpoint, e.g.
	// "wss://<vps-ip>:8443/sidechannel". The host part must match the
	// server certificate's IP SAN.
	URL string

	// CACertPath is the CA that signed the server cert (RootCAs).
	CACertPath string
	// ClientCert / ClientKey are the edge's own cert+key, presented
	// for mTLS.
	ClientCert string
	ClientKey  string

	Log *slog.Logger

	// PingInterval overrides the app-level ping cadence. Zero uses
	// defaultPingInterval. Tests set a short value.
	PingInterval time.Duration

	// InitialBackoff overrides the first reconnect delay. Zero uses
	// defaultInitialBackoff. Doubles up to maxBackoff, resets on a
	// successful connection. Tests set a short value.
	InitialBackoff time.Duration

	// OnPong is a test hook fired when a pong is received. nil in
	// production.
	OnPong func()

	// OnRequestPublish, when set, is invoked (in its own goroutine)
	// when the cloud sends a request_publish frame. The edge wiring
	// implements it: authorise the stream, issue a publish token,
	// Send a start_publish, and kick the StreamPublisher. iceServers
	// carries the cloud-minted TURN credentials from the frame (nil ->
	// host-only). A nil callback ignores the frame.
	OnRequestPublish func(streamID string, iceServers []streampublish.ICEServer)

	// OnRequestStop, when set, is invoked (in its own goroutine) when the cloud
	// sends a request_stop frame: the last WHEP subscriber for streamID left.
	// The edge wiring implements it as EdgePublisher.StopPublish - tear the
	// publish bridge down and Send a stop_publish back - so the profile row
	// falls back to idle instead of lingering "active" (the S20
	// cloud-row-never-clears bug). The symmetric counterpart to
	// OnRequestPublish. A nil callback ignores the frame.
	OnRequestStop func(streamID string)

	// OnTURNEvent / OnTURNStats, when set, receive the cloud-forwarded
	// TURN telemetry (turn_event / turn_stats frames). The edge wiring
	// points them at the turnstore writer (persist) and snapshot holder
	// (live stats). Both are expected to be non-blocking (a buffered
	// Submit / a quick mutex), so they run inline on the read loop and
	// preserve frame order. A nil callback ignores the frame.
	OnTURNEvent func(turnstore.Event)
	OnTURNStats func(turnstore.Snapshot)

	// OnStreamStats, when set, receives the cloud-forwarded live cloud-viewer
	// snapshot (stream_stats frame): the per-stream WHEP consumer counts. The
	// edge wiring points it at the streamstore snapshot holder. Expected to be
	// non-blocking (a quick mutex), so it runs inline on the read loop and
	// preserves frame order. A nil callback ignores the frame. (S20)
	OnStreamStats func(streamstore.Snapshot)

	// OnBundleRequest, when set, answers a cloud bundle_request (Saison
	// 19-11): given the raw viewer Bearer it resolves the viewer MAC and mints
	// a sid-bound egress token, returning mac + egressToken + the token TTL in
	// seconds, or an error when auth/mint fails. The edge wiring points it at
	// resolveViewer + the egress issuer. Runs in its own goroutine (it hits
	// the DB + Argon2id verify) so it never blocks the read loop; the concrete
	// error is logged here and only a bare "unauthorized" crosses back to the
	// cloud (no oracle). A nil callback ignores the frame (the cloud times out
	// -> 503).
	OnBundleRequest func(credential string) (mac, egressToken string, expiresIn int, edgeWHEPURL string, err error)

	// OnHTTPRequest, when set, answers a cloud http_request (Saison 19-27, the
	// generic control relay): it runs the relayed request through the edge's
	// OWN httpserver mux (requireViewerAuth + the unchanged handler) and
	// returns the captured response. The edge wiring points it at
	// httpserver.Server.ServeRelayed. Runs in its own goroutine (it may hit the
	// DB or the UA-API) so it never blocks the read loop. A non-nil error means
	// the edge could not RUN the request (mechanism failure) -> framed as
	// http_reply.Error (cloud 503); a normal HTTP status (incl. 401) rides the
	// returned RelayResponse. A nil callback ignores the frame.
	OnHTTPRequest func(ctx context.Context, req RelayRequest) (RelayResponse, error)
}

// Client is the edge-side dialer. It keeps a single mTLS WebSocket to
// the cloud, reconnecting with exponential backoff, and sends an
// app-level ping on an interval as the visible proof of life.
//
// Run never blocks the edge (Grundregel): it loops in its own
// goroutine, logs failures, retries, and returns only when ctx is
// cancelled. A cloud outage never reaches the local subsystems.
type Client struct {
	opts       ClientOptions
	httpClient *http.Client
	log        *slog.Logger
	pingEvery  time.Duration
	initialBO  time.Duration
	// send carries outgoing frames (start_publish/stop_publish) to the
	// single writer in connectAndServe. Buffered + non-blocking so a
	// caller never stalls on a down link.
	send chan Envelope

	// pending correlates an in-flight RequestICE (by request_id) with the
	// ice_servers reply the read loop delivers. Guarded by mu. The reply
	// chans are buffered (cap 1) and deleted on RequestICE return, so the
	// read loop never blocks and timed-out entries do not leak.
	mu      sync.Mutex
	pending map[string]chan iceReply
}

// NewClient validates options and loads the TLS material.
func NewClient(opts ClientOptions) (*Client, error) {
	if opts.Log == nil {
		return nil, errors.New("sidechannel: client Log must not be nil")
	}
	if opts.URL == "" {
		return nil, errors.New("sidechannel: client URL must not be empty")
	}
	tlsCfg, err := clientTLSConfig(opts.CACertPath, opts.ClientCert, opts.ClientKey)
	if err != nil {
		return nil, err
	}
	pingEvery := opts.PingInterval
	if pingEvery <= 0 {
		pingEvery = defaultPingInterval
	}
	initialBO := opts.InitialBackoff
	if initialBO <= 0 {
		initialBO = defaultInitialBackoff
	}
	return &Client{
		opts: opts,
		httpClient: &http.Client{
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
		log:       opts.Log,
		pingEvery: pingEvery,
		initialBO: initialBO,
		send:      make(chan Envelope, sendQueueSize),
		pending:   make(map[string]chan iceReply),
	}, nil
}

// Send enqueues a frame for the current connection. Non-blocking: if
// the queue is full or the link is down the frame is dropped with a
// warn. The cloud link is additive, so a dropped control frame must
// never stall the edge.
func (c *Client) Send(env Envelope) {
	select {
	case c.send <- env:
	default:
		c.log.Warn("sidechannel send queue full, dropping frame",
			"type", env.Type, "stream_id", env.StreamID)
	}
}

// RequestICE asks the cloud to mint a fresh set of subscriber ICE servers and
// waits for the matching ice_servers reply (the request_ice/ice_servers RPC).
// A random request_id correlates the reply back to this call. Returns the
// neutral ICE result (servers + the cloud's public WHEP base, "" when the
// cloud has no public WHEP listener), ErrNoICEServers if the cloud had no
// minter, or ctx.Err() if no reply arrives before ctx (or the internal
// fallback) elapses or the link is down. The cloud link is additive: callers
// treat any error as "remote unavailable" (the local LAN path is unaffected).
func (c *Client) RequestICE(ctx context.Context) (streampublish.ICEResult, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, iceRequestTimeout)
		defer cancel()
	}
	id, err := newRequestID()
	if err != nil {
		return streampublish.ICEResult{}, fmt.Errorf("sidechannel: request id: %w", err)
	}

	// Buffered cap-1 so the read loop's delivery never blocks; deleted on
	// return so a timed-out request leaves no pending entry behind.
	ch := make(chan iceReply, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	// Fire-and-forget enqueue: a dropped frame (full queue / down link) just
	// means no reply arrives and ctx elapses -> error -> caller treats it as
	// remote-unavailable.
	c.Send(Envelope{Type: TypeRequestICE, RequestID: id})

	select {
	case reply := <-ch:
		if len(reply.servers) == 0 {
			return streampublish.ICEResult{}, ErrNoICEServers
		}
		return streampublish.ICEResult{Servers: reply.servers, WHEPBaseURL: reply.whepBaseURL}, nil
	case <-ctx.Done():
		return streampublish.ICEResult{}, ctx.Err()
	}
}

// deliverICEReply routes an ice_servers frame to the waiting RequestICE call
// by RequestID. A missing entry means the caller already timed out and
// deregistered -> log and drop. The send is non-blocking (cap-1 chan), so the
// read loop is never stalled by a slow or vanished caller.
func (c *Client) deliverICEReply(env Envelope) {
	c.mu.Lock()
	ch, ok := c.pending[env.RequestID]
	c.mu.Unlock()
	if !ok {
		c.log.Warn("sidechannel ice_servers for unknown/expired request, dropping",
			"request_id", env.RequestID, "ice_servers", len(env.ICEServers))
		return
	}
	select {
	case ch <- iceReply{servers: env.ICEServers, expiresIn: env.ExpiresInSeconds, whepBaseURL: env.WHEPBaseURL}:
	default:
		c.log.Warn("sidechannel duplicate ice_servers, dropping", "request_id", env.RequestID)
	}
}

// newRequestID returns a short random hex correlation id for request_ice.
func newRequestID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Run loops until ctx is cancelled, reconnecting with exponential
// backoff (initial..30s, reset on a successful connection).
func (c *Client) Run(ctx context.Context) error {
	backoff := c.initialBO
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := c.connectAndServe(ctx, func() { backoff = c.initialBO })
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.log.Warn("sidechannel disconnected, reconnecting", "err", err, "backoff", backoff.String())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// connectAndServe dials once and serves until the connection drops or
// ctx is cancelled. onConnected is called right after a successful
// dial so Run can reset its backoff.
func (c *Client) connectAndServe(ctx context.Context, onConnected func()) error {
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	conn, _, err := websocket.Dial(dialCtx, c.opts.URL, &websocket.DialOptions{
		HTTPClient: c.httpClient,
	})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()
	onConnected()
	c.log.Info("sidechannel connected", "url", c.opts.URL)

	// Reader goroutine: handles pongs now, cloud-initiated cargo
	// later. Errors flow back to the main loop, which returns to
	// trigger a reconnect.
	readErr := make(chan error, 1)
	go func() {
		for {
			var env Envelope
			if err := wsjson.Read(ctx, conn, &env); err != nil {
				readErr <- err
				return
			}
			switch env.Type {
			case TypePong:
				c.log.Info("sidechannel pong received")
				if c.opts.OnPong != nil {
					c.opts.OnPong()
				}
			case TypeRequestPublish:
				c.log.Info("sidechannel request_publish received", "stream_id", env.StreamID, "ice_servers", len(env.ICEServers))
				if c.opts.OnRequestPublish != nil {
					sid := env.StreamID
					ice := env.ICEServers
					// Own goroutine: the edge business logic (authz,
					// token, StreamPublisher) must never block the
					// read loop.
					go c.opts.OnRequestPublish(sid, ice)
				}
			case TypeRequestStop:
				c.log.Info("sidechannel request_stop received", "stream_id", env.StreamID, "reason", env.Reason)
				if c.opts.OnRequestStop != nil {
					sid := env.StreamID
					// Own goroutine, mirroring request_publish: the edge
					// teardown (StopPublish -> Send stop_publish + tear the
					// bridge down) must never block the read loop.
					go c.opts.OnRequestStop(sid)
				}
			case TypeICEServers:
				// Reply to an edge-initiated RequestICE; route it to the
				// waiting caller by RequestID. Quick map lookup + buffered
				// send, so run inline on the read loop.
				c.deliverICEReply(env)
			case TypeBundleRequest:
				// Cloud relayed a remote subscriber's stream-start. Resolve +
				// mint in a goroutine (it hits the DB + Argon2id), then reply -
				// never block the read loop. nil callback -> ignore (the cloud
				// times out -> 503).
				if c.opts.OnBundleRequest != nil {
					cred := env.Credential
					id := env.RequestID
					go func() {
						mac, tok, ttl, edgeURL, err := c.opts.OnBundleRequest(cred)
						reply := Envelope{Type: TypeBundleReply, RequestID: id}
						if err != nil {
							// Concrete reason logged on the edge ONLY; a fixed,
							// non-revealing error crosses back (no oracle).
							c.log.Warn("sidechannel bundle_request rejected", "request_id", id, "err", err)
							reply.Error = "unauthorized"
						} else {
							reply.MAC = mac
							reply.EgressToken = tok
							reply.ExpiresInSeconds = ttl
							reply.EdgeWHEPURL = edgeURL // Saison 19-41: LAN-direct URL (may be empty)
						}
						c.Send(reply)
					}()
				}
			case TypeHTTPRequest:
				// Cloud relayed a control call; run it through the edge's own mux
				// in a goroutine (it may hit the DB or the UA-API), then frame the
				// captured response. A mechanism failure crosses back as Error
				// (cloud 503); a normal HTTP status (incl. 401) rides Status. nil
				// callback -> ignore (the cloud times out -> 503).
				if c.opts.OnHTTPRequest != nil {
					req := RelayRequest{
						Method:   env.Method,
						Path:     env.Path,
						RawQuery: env.RawQuery,
						Header:   env.HTTPHeader,
						Body:     env.Body,
					}
					id := env.RequestID
					go func() {
						rctx, cancel := context.WithTimeout(context.Background(), relayServeTimeout)
						defer cancel()
						reply := Envelope{Type: TypeHTTPReply, RequestID: id}
						resp, err := c.opts.OnHTTPRequest(rctx, req)
						if err != nil {
							c.log.Warn("sidechannel http_request could not run",
								"request_id", id, "path", req.Path, "err", err)
							reply.Error = "relay failed"
						} else {
							reply.Status = resp.Status
							reply.HTTPHeader = resp.Header
							reply.Body = resp.Body
						}
						c.Send(reply)
					}()
				}
			case TypeTURNEvent:
				// Cloud-forwarded TURN history event. The callback is a
				// non-blocking turnstore Submit, so run it inline (this
				// also preserves frame order).
				if c.opts.OnTURNEvent != nil && env.TURNEvent != nil {
					c.opts.OnTURNEvent(*env.TURNEvent)
				}
			case TypeTURNStats:
				// Periodic live snapshot; the callback just stores it
				// (quick mutex), so run it inline.
				if c.opts.OnTURNStats != nil && env.TURNStats != nil {
					c.opts.OnTURNStats(*env.TURNStats)
				}
			case TypeStreamStats:
				// Periodic live cloud-viewer snapshot; the callback just
				// stores it (quick mutex), so run it inline. (S20)
				if c.opts.OnStreamStats != nil && env.StreamStats != nil {
					c.opts.OnStreamStats(*env.StreamStats)
				}
			default:
				c.log.Warn("sidechannel unknown message type", "type", env.Type)
			}
		}
	}()

	ticker := time.NewTicker(c.pingEvery)
	defer ticker.Stop()

	// Initial ping immediately, so the proof of life appears without
	// waiting a full interval.
	if err := c.writeEnvelope(ctx, conn, Envelope{Type: TypePing}); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			_ = conn.Close(websocket.StatusNormalClosure, "shutdown")
			return ctx.Err()
		case err := <-readErr:
			return fmt.Errorf("read: %w", err)
		case env := <-c.send:
			// Outgoing start_publish/stop_publish from the edge
			// business logic. Single writer (this loop), so writes
			// stay serialised as coder/websocket requires.
			if err := c.writeEnvelope(ctx, conn, env); err != nil {
				return err
			}
		case <-ticker.C:
			if err := c.writeEnvelope(ctx, conn, Envelope{Type: TypePing}); err != nil {
				return err
			}
		}
	}
}

// writeEnvelope is the single serialised writer for the connection.
func (c *Client) writeEnvelope(ctx context.Context, conn *websocket.Conn, env Envelope) error {
	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := wsjson.Write(writeCtx, conn, env); err != nil {
		return fmt.Errorf("write %s: %w", env.Type, err)
	}
	c.log.Info("sidechannel frame sent", "type", env.Type, "stream_id", env.StreamID)
	return nil
}

func clientTLSConfig(caPath, certPath, keyPath string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("sidechannel: load client keypair: %w", err)
	}
	pool, err := caPool(caPath)
	if err != nil {
		return nil, err
	}
	// No InsecureSkipVerify: the server cert carries an IP SAN that
	// matches the dial host, so standard verification against RootCAs
	// applies in full.
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
