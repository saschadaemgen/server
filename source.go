package stream

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// Source pulls an H.264 video track from an RTSP/RTSPS endpoint and exposes
// it as a single shared pion track that any number of [Server] peers can
// attach to.
//
// The concrete type is what callers hold today. A future revision will lift
// a VideoSource interface above it (so MJPEG/file/test sources can plug in
// the same way), but for the spike the concrete struct is enough.
type Source struct {
	rtspURL string
	logger  *log.Logger

	// insecureTLS disables certificate verification on the RTSPS connection.
	// The UDM/UniFi cert has no IP-SAN, so default Go verify always fails.
	// True for the spike. Future: pin the UDM CA like the carvilon UA client does.
	insecureTLS bool

	mu     sync.Mutex
	client *gortsplib.Client
	track  *webrtc.TrackLocalStaticRTP
}

// SourceOptions configures a [Source].
type SourceOptions struct {
	// RTSPURL is the full rtsps://... URL including any query parameters
	// (e.g. ?enableSrtp). MUST be supplied at runtime — never hard-code.
	RTSPURL string

	// Logger receives diagnostic output. If nil, the default logger is used.
	Logger *log.Logger

	// InsecureSkipTLSVerify skips certificate validation. Required against
	// the UDM today; revisit when CA pinning is added.
	InsecureSkipTLSVerify bool
}

// NewSource constructs a Source. It does not connect — call [Source.Start].
func NewSource(opts SourceOptions) (*Source, error) {
	if opts.RTSPURL == "" {
		return nil, errors.New("stream: RTSPURL is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &Source{
		rtspURL:     opts.RTSPURL,
		logger:      logger,
		insecureTLS: opts.InsecureSkipTLSVerify,
	}, nil
}

// Track returns the shared pion video track. Safe to call after [Source.Start]
// returns successfully. Multiple PeerConnections may attach the same track.
func (s *Source) Track() *webrtc.TrackLocalStaticRTP {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.track
}

// Start connects to the RTSP source, negotiates the H.264 track, and begins
// forwarding RTP packets into the internal pion track. It returns once
// playback has been requested; packets continue flowing in background
// goroutines owned by gortsplib until [Source.Close] is called or ctx is
// cancelled.
func (s *Source) Start(ctx context.Context) error {
	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video",
		"carvilon-intercom",
	)
	if err != nil {
		return fmt.Errorf("stream: create local track: %w", err)
	}

	u, err := base.ParseURL(s.rtspURL)
	if err != nil {
		return fmt.Errorf("stream: parse rtsp url: %w", err)
	}

	client := &gortsplib.Client{
		Scheme: u.Scheme,
		Host:   u.Host,
	}
	if s.insecureTLS {
		client.TLSConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // spike, see doc
	}

	if err := client.Start(); err != nil {
		return fmt.Errorf("stream: rtsp dial %s://%s: %w", u.Scheme, u.Host, err)
	}

	desc, _, err := client.Describe(u)
	if err != nil {
		client.Close()
		return fmt.Errorf("stream: rtsp describe: %w", err)
	}

	var h264 *format.H264
	medi := desc.FindFormat(&h264)
	if medi == nil {
		client.Close()
		return errors.New("stream: no H.264 track in media description")
	}
	s.logger.Printf("stream: found H.264 track (payload type %d)", h264.PayloadType())

	if _, err := client.Setup(desc.BaseURL, medi, 0, 0); err != nil {
		client.Close()
		return fmt.Errorf("stream: rtsp setup: %w", err)
	}

	client.OnPacketRTP(medi, h264, func(pkt *rtp.Packet) {
		// TrackLocalStaticRTP.WriteRTP rewrites SSRC/payload-type per binding,
		// so we forward the gortsplib packet as-is. With no peers attached the
		// write is a no-op — drop-statt-buffer falls out for free.
		if err := track.WriteRTP(pkt); err != nil && !errors.Is(err, errClosedTrack) {
			// In single-viewer-spike scope: log and keep going. Audible at
			// console if something is structurally wrong, but never fatal.
			s.logger.Printf("stream: forward rtp: %v", err)
		}
	})

	if _, err := client.Play(nil); err != nil {
		client.Close()
		return fmt.Errorf("stream: rtsp play: %w", err)
	}

	s.mu.Lock()
	s.client = client
	s.track = track
	s.mu.Unlock()

	go s.wait(ctx, client)
	return nil
}

// Close stops the RTSP client. Safe to call multiple times.
func (s *Source) Close() error {
	s.mu.Lock()
	c := s.client
	s.client = nil
	s.mu.Unlock()
	if c == nil {
		return nil
	}
	c.Close()
	return nil
}

// wait observes either the gortsplib client exiting (Wait returns) or the
// caller-supplied context being cancelled, and tears down whichever side is
// still alive.
func (s *Source) wait(ctx context.Context, c *gortsplib.Client) {
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			s.logger.Printf("stream: rtsp client stopped: %v", err)
		}
	case <-ctx.Done():
		c.Close()
	}
}

// errClosedTrack is a sentinel for "track has no bindings yet" / "track was
// closed" — pion does not currently surface a typed error here, but we keep
// the name so we can swap in the typed value later without touching callers.
var errClosedTrack = errors.New("track closed")
