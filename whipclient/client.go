// Package whipclient implements a WHIP (RFC 9725) publisher client.
//
// It is designed to be embedded in carvilon-edge as the concrete
// StreamPublisher implementation: StartPublish/StopPublish are
// non-blocking, the actual WebRTC negotiation and publish run in
// per-stream worker goroutines. The caller is the carvilon side-channel
// read-loop, which must never block — hence the worker pattern.
//
// The client does not own the media source. The caller provides a
// [TrackSourceFunc] in the [Config]; the client invokes it lazily when
// StartPublish fires for a given streamID. On StopPublish or ICE
// failure the client closes the PeerConnection, which detaches the
// track and (cloud-side) triggers the ICE-state cleanup in the WHIP
// server — there is deliberately no HTTP DELETE.
package whipclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"carvilon.local/stream/internal/icedebug"
)

// gatherTimeout bounds the wait for ICE candidate gathering before the
// offer is POSTed. Push-side gathers host candidates near-instantly;
// the timeout only guards a stuck stack.
const gatherTimeout = 5 * time.Second

// defaultHTTPTimeout is the WHIP POST timeout when the caller does not
// supply its own HTTPClient.
const defaultHTTPTimeout = 15 * time.Second

// TrackSourceFunc returns the track to publish for the given streamID,
// plus a stop function that releases the underlying source. It is
// invoked once per StartPublish in the worker goroutine.
//
// S2-06: the track is returned as the [webrtc.TrackLocal] interface
// (not a concrete *TrackLocalStaticRTP), so an in-process source such
// as [stream.Server.TrackForStream] — which yields a
// TrackLocalStaticSample — can satisfy it directly. The stop function
// is called by the worker on teardown (StopPublish / ICE failure /
// Close); for the TrackForStream source that unsubscribes from the hub
// and releases the shared upstream camera pull. It is the
// bandwidth-critical hook: without it the pull would outlive the
// publish. stop may be nil (the worker guards it).
type TrackSourceFunc func(streamID string) (track webrtc.TrackLocal, stop func(), err error)

// Config configures a [Client].
type Config struct {
	// TrackSource is required. nil -> New returns an error.
	TrackSource TrackSourceFunc

	// TLSConfig is used for the HTTPS connection to the WHIP server. If
	// nil, the system root CA pool is used. For the CARVILON cloud
	// (Mini-CA), the caller MUST provide a TLSConfig with RootCAs set to
	// a CertPool containing ca.crt. Ignored if HTTPClient is set.
	TLSConfig *tls.Config

	// HTTPClient is used for the WHIP POST. If nil, a default client
	// (Timeout: 15s, using TLSConfig) is constructed.
	HTTPClient *http.Client

	// Logger is required (no nil default; the caller chooses the sink).
	Logger *log.Logger

	// OnICEState, when non-nil, is called on every ICE connection-state
	// change with a structured event, IN ADDITION to the existing log line,
	// so the embedding module (carvilon admin) can surface ICE history.
	// OPTIONAL: nil -> log-only (today's behaviour; no break). Called from
	// pion's ICE goroutine; must be safe for concurrent use. It carries
	// only stdlib types (no pion type) - open-core, like the TURN naht.
	OnICEState func(ICEStateEvent)
}

// ICEStateEvent is a structured ICE connection-state transition for the
// optional Config.OnICEState callback. Stdlib types only (no pion type), so
// the embedding module's public build stays pion-free.
type ICEStateEvent struct {
	StreamID   string
	State      string        // pion ICEConnectionState as a string: "checking" | "connected" | "failed" | "closed" | ...
	Time       time.Time     // when the transition was observed
	SinceStart time.Duration // elapsed since the worker created the PeerConnection
}

// Client is a non-blocking WHIP publisher. Construct with [New].
type Client struct {
	cfg        Config
	httpClient *http.Client

	mu       sync.Mutex
	sessions map[string]*session // streamID -> live session
}

type session struct {
	pc       *webrtc.PeerConnection
	location string             // Location header from 201 (for a future DELETE)
	cancel   context.CancelFunc // signals the worker to stop
	done     chan struct{}      // closed when the worker goroutine exits
}

// New validates the config and returns a ready Client.
func New(cfg Config) (*Client, error) {
	if cfg.TrackSource == nil {
		return nil, errors.New("whipclient: TrackSource is required")
	}
	if cfg.Logger == nil {
		return nil, errors.New("whipclient: Logger is required")
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		transport := &http.Transport{}
		if cfg.TLSConfig != nil {
			transport.TLSClientConfig = cfg.TLSConfig
		}
		httpClient = &http.Client{Timeout: defaultHTTPTimeout, Transport: transport}
	}
	return &Client{
		cfg:        cfg,
		httpClient: httpClient,
		sessions:   make(map[string]*session),
	}, nil
}

// StartPublish initiates a WHIP push for streamID. Returns immediately;
// the actual publish runs in a worker goroutine. If a session for
// streamID already exists, the call is logged and ignored (no
// double-publish).
//
// iceServers (S3 TURN) is set on the PeerConnection so the edge can form
// relay candidates through CGNAT. It carries the TURN URL(s) + the
// short-lived REST credentials the cloud minted and handed over via the
// side-channel start_publish frame. Empty/nil -> host candidates only
// (the pre-TURN behaviour).
func (c *Client) StartPublish(streamID, publishToken, cloudWhipURL string, iceServers []webrtc.ICEServer) {
	c.mu.Lock()
	if _, exists := c.sessions[streamID]; exists {
		c.mu.Unlock()
		c.cfg.Logger.Printf("whipclient: publish already active for streamID=%s (ignored)", streamID)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	sess := &session{cancel: cancel, done: make(chan struct{})}
	c.sessions[streamID] = sess
	c.mu.Unlock()

	go c.runPublish(ctx, streamID, publishToken, cloudWhipURL, iceServers, sess)
}

// StopPublish terminates the worker for streamID. Returns immediately;
// teardown runs in the worker goroutine. No-op if no session exists.
func (c *Client) StopPublish(streamID string) {
	c.mu.Lock()
	sess, ok := c.sessions[streamID]
	c.mu.Unlock()
	if !ok {
		c.cfg.Logger.Printf("whipclient: stop for unknown streamID=%s (no-op)", streamID)
		return
	}
	sess.cancel() // worker observes ctx.Done(), tears down, removes itself
}

// Close terminates all active sessions and blocks until each worker
// exits. Used at shutdown.
func (c *Client) Close() error {
	c.mu.Lock()
	live := make([]*session, 0, len(c.sessions))
	for _, s := range c.sessions {
		live = append(live, s)
	}
	c.mu.Unlock()

	for _, s := range live {
		s.cancel()
	}
	for _, s := range live {
		<-s.done
	}
	return nil
}

// runPublish is the per-stream worker. It performs the full WHIP
// handshake and then parks on ctx.Done() until StopPublish/Close or an
// ICE failure cancels it. On any exit path it closes the PeerConnection
// (which cloud-side triggers ICE-based session cleanup), removes itself
// from the session map, and signals done.
func (c *Client) runPublish(ctx context.Context, streamID, publishToken, cloudWhipURL string, iceServers []webrtc.ICEServer, sess *session) {
	defer close(sess.done)
	defer c.removeSession(streamID)
	defer sess.cancel() // release the context on every exit path

	track, stopTrack, err := c.cfg.TrackSource(streamID)
	if err != nil {
		c.cfg.Logger.Printf("whipclient: track source failed for streamID=%s: %v", streamID, err)
		return
	}
	// S2-06: release the source on every exit path. Registered before
	// the PeerConnection defer so (LIFO) pc.Close runs first (detaching
	// the track), then stopTrack tears the source down — releasing the
	// shared upstream pull. nil-safe per the TrackSourceFunc contract.
	if stopTrack != nil {
		defer stopTrack()
	}

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: iceServers})
	if err != nil {
		c.cfg.Logger.Printf("whipclient: new peer connection for streamID=%s: %v", streamID, err)
		return
	}
	defer func() { _ = pc.Close() }()
	c.mu.Lock()
	sess.pc = pc
	c.mu.Unlock()

	if _, err := pc.AddTrack(track); err != nil {
		c.cfg.Logger.Printf("whipclient: add track for streamID=%s: %v", streamID, err)
		return
	}

	// S3 ICE befund: opt-in masked candidate + state logging
	// (CARVILON_ICE_DEBUG). Purely additive; no-op when the flag is off.
	icedebug.AttachCandidateLogging(pc, c.cfg.Logger, "whipclient streamID="+streamID)
	iceTracker := icedebug.NewStateTracker(c.cfg.Logger, "whipclient streamID="+streamID)
	iceStart := time.Now() // origin for ICEStateEvent.SinceStart

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		c.cfg.Logger.Printf("whipclient: streamID=%s ICE state=%s", streamID, state)
		iceTracker.Log(state)
		// S3 telemetry: optional structured event for the admin history,
		// in addition to the log line. nil callback -> log-only.
		if c.cfg.OnICEState != nil {
			c.cfg.OnICEState(ICEStateEvent{
				StreamID:   streamID,
				State:      state.String(),
				Time:       time.Now(),
				SinceStart: time.Since(iceStart),
			})
		}
		switch state {
		case webrtc.ICEConnectionStateFailed,
			webrtc.ICEConnectionStateDisconnected,
			webrtc.ICEConnectionStateClosed:
			sess.cancel()
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		c.cfg.Logger.Printf("whipclient: create offer for streamID=%s: %v", streamID, err)
		return
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		c.cfg.Logger.Printf("whipclient: set local description for streamID=%s: %v", streamID, err)
		return
	}
	select {
	case <-gather:
	case <-ctx.Done():
		return
	case <-time.After(gatherTimeout):
		c.cfg.Logger.Printf("whipclient: ICE gathering timed out for streamID=%s", streamID)
		return
	}

	answer, location, err := c.postOffer(ctx, streamID, publishToken, cloudWhipURL, pc.LocalDescription().SDP)
	if err != nil {
		c.cfg.Logger.Printf("whipclient: publish failed for streamID=%s: %v", streamID, err)
		return
	}
	c.mu.Lock()
	sess.location = location
	c.mu.Unlock()

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  answer,
	}); err != nil {
		c.cfg.Logger.Printf("whipclient: set remote description for streamID=%s: %v", streamID, err)
		return
	}

	c.cfg.Logger.Printf("whipclient: publish started: streamID=%s session=%s", streamID, sessionIDFromLocation(location))

	// Park until cancelled (StopPublish / Close / ICE failure).
	<-ctx.Done()
}

// postOffer performs the WHIP POST and returns the SDP answer + Location
// header on a 201. The bearer token is set on the request but NEVER
// logged (master memory rule).
func (c *Client) postOffer(ctx context.Context, streamID, publishToken, cloudWhipURL, offerSDP string) (answer, location string, err error) {
	reqURL := strings.TrimRight(cloudWhipURL, "/") + "/" + streamID
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(offerSDP))
	if err != nil {
		return "", "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+publishToken)
	req.Header.Set("Content-Type", "application/sdp")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", "", fmt.Errorf("read answer: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		return "", "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return string(body), resp.Header.Get("Location"), nil
}

// removeSession drops streamID from the session map. Idempotent.
func (c *Client) removeSession(streamID string) {
	c.mu.Lock()
	delete(c.sessions, streamID)
	c.mu.Unlock()
}

// sessionIDFromLocation pulls the trailing path segment out of a WHIP
// Location header (".../session/<id>"). Returns "" if there's no slash.
func sessionIDFromLocation(location string) string {
	if i := strings.LastIndex(location, "/"); i >= 0 {
		return location[i+1:]
	}
	return ""
}
