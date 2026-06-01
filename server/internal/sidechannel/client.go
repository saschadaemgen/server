package sidechannel

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"carvilon.local/server/internal/streampublish"
)

const (
	defaultInitialBackoff = time.Second
	maxBackoff            = 30 * time.Second
	defaultPingInterval   = 15 * time.Second
	dialTimeout           = 10 * time.Second
	writeTimeout          = 10 * time.Second
	sendQueueSize         = 16
)

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
