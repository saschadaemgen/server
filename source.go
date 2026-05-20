package stream

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// Source pulls an H.264 video track from an RTSP/RTSPS endpoint, reassembles
// it into access units, and exposes them to pion as a single shared sample
// track that any number of [Server] peers can attach to.
//
// We deliberately go through samples rather than forwarding raw RTP. The
// UA-Intercom packs its RTP payload in a way pion's writer cannot
// round-trip (S1-02 failed when forwarding the raw packets). By repacketizing
// the H.264 access units ourselves through pion, the upstream RTP layout is
// irrelevant — pion produces a clean wire format and handles SRTP without
// surprises.
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
	track  *webrtc.TrackLocalStaticSample

	drops *forwardDropCounter
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
		drops:       &forwardDropCounter{logger: logger},
	}, nil
}

// Track returns the shared pion sample track. Safe to call after
// [Source.Start] returns successfully. Multiple PeerConnections may attach
// the same track.
func (s *Source) Track() *webrtc.TrackLocalStaticSample {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.track
}

// Start connects to the RTSP source, negotiates the H.264 track, and begins
// reassembling access units into samples for the pion track. It returns
// once playback has been requested; samples continue flowing in background
// goroutines owned by gortsplib until [Source.Close] is called or ctx is
// cancelled.
func (s *Source) Start(ctx context.Context) error {
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: h264ClockRate},
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

	var h264Fmt *format.H264
	medi := desc.FindFormat(&h264Fmt)
	if medi == nil {
		client.Close()
		return errors.New("stream: no H.264 track in media description")
	}
	s.logger.Printf("stream: found H.264 track (payload type %d, packetization-mode %d)",
		h264Fmt.PayloadType(), h264Fmt.PacketizationMode)

	rtpDec, err := h264Fmt.CreateDecoder()
	if err != nil {
		client.Close()
		return fmt.Errorf("stream: create H.264 RTP decoder: %w", err)
	}

	if _, err := client.Setup(desc.BaseURL, medi, 0, 0); err != nil {
		client.Close()
		return fmt.Errorf("stream: rtsp setup: %w", err)
	}

	// Forward-loop state. The OnPacketRTP callback runs in a single gortsplib
	// goroutine, so these are touched from one thread — no mutex needed.
	var (
		seenIDR    bool
		prevPTSSet bool
		prevPTS    int64
	)

	client.OnPacketRTP(medi, h264Fmt, func(pkt *rtp.Packet) {
		// gortsplib reassembles FU-A fragments and STAP-A bundles for us and
		// returns a complete access unit (or one of the two expected
		// "waiting" sentinels) per call.
		au, err := rtpDec.Decode(pkt)
		if err != nil {
			if errors.Is(err, rtph264.ErrMorePacketsNeeded) ||
				errors.Is(err, rtph264.ErrNonStartingPacketAndNoPrevious) {
				// Normal during fragmentation / mid-stream connect; not a drop.
				return
			}
			s.drops.record(err)
			return
		}

		pts, ok := client.PacketPTS(medi, pkt)
		if !ok {
			// Timestamp not yet synchronised — drop quietly until rtptime
			// has anchored to a leading track.
			return
		}

		// Wait for the first IDR-containing AU. Anything before that is a
		// P-frame that cannot be decoded standalone; sending it would just
		// give the browser garbage until the next keyframe arrives anyway.
		isKeyframe := auContainsIDR(au)
		if !seenIDR {
			if !isKeyframe {
				return
			}
			seenIDR = true
			s.logger.Printf("stream: first IDR received, starting sample output")
		}

		// Be defensive about SPS/PPS. Some cameras ship them only in the SDP
		// (gortsplib stashes those into h264Fmt.SPS/PPS); some emit them
		// inline at every IDR. We prepend the SDP copies on every IDR that
		// lacks them — the browser decoder needs them to lock on.
		if isKeyframe {
			au = prependParamSets(au, h264Fmt)
		}

		// Sample.Duration tells pion how much to advance the outbound RTP
		// timestamp BEFORE the *next* sample. Using the gap to the previous
		// AU as a proxy is exact for constant frame rate and good enough for
		// variable rate — the alternative (buffer one frame to know the real
		// gap) would add a frame of latency we do not need today.
		dur := frameDuration(pts, prevPTS, prevPTSSet)
		prevPTS = pts
		prevPTSSet = true

		if err := track.WriteSample(media.Sample{
			Data:     annexBMarshal(au),
			Duration: dur,
		}); err != nil {
			s.drops.record(err)
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

// forwardDropCounter rate-limits "frame dropped on forward" log lines.
//
// Live video prefers freshness over completeness: drop the frame, never the
// loop. We still want one line in the log when something is structurally
// wrong, but per-packet spam buries everything else. So: count drops
// silently and emit at most one summary per second, including the last
// error seen.
type forwardDropCounter struct {
	logger *log.Logger

	mu      sync.Mutex
	lastAt  time.Time
	pending int
}

func (d *forwardDropCounter) record(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.pending++
	if time.Since(d.lastAt) < time.Second {
		return
	}
	d.logger.Printf("stream: forward dropped %d frame(s); last err: %v", d.pending, err)
	d.lastAt = time.Now()
	d.pending = 0
}

// --- H.264 helpers (Annex-B framing, NAL type checks) ---
//
// We intentionally roll these by hand rather than pulling in mediacommon
// directly. The dependency doctrine for the streaming-server keeps only
// pion/* and gortsplib at the top level; mediacommon is an indirect dep we
// don't want to promote. The actual logic is a few lines of trivial byte
// manipulation.

const h264ClockRate = 90000

const (
	h264NALTypeIDR byte = 5
	h264NALTypeSPS byte = 7
	h264NALTypePPS byte = 8
)

func h264NALType(nalu []byte) byte {
	if len(nalu) == 0 {
		return 0
	}
	return nalu[0] & 0x1F
}

func auContainsIDR(au [][]byte) bool {
	for _, nalu := range au {
		if h264NALType(nalu) == h264NALTypeIDR {
			return true
		}
	}
	return false
}

func auHasType(au [][]byte, want byte) bool {
	for _, nalu := range au {
		if h264NALType(nalu) == want {
			return true
		}
	}
	return false
}

// prependParamSets ensures the AU starts with SPS and PPS so a fresh decoder
// (or a viewer that just connected and is keyframing from this AU) can lock
// on. If the AU already contains SPS/PPS, or gortsplib does not have a copy
// from the SDP, nothing is prepended for that NAL type.
func prependParamSets(au [][]byte, h264Fmt *format.H264) [][]byte {
	sps, pps := h264Fmt.SafeParams()
	var prepend [][]byte
	if len(sps) > 0 && !auHasType(au, h264NALTypeSPS) {
		prepend = append(prepend, sps)
	}
	if len(pps) > 0 && !auHasType(au, h264NALTypePPS) {
		prepend = append(prepend, pps)
	}
	if len(prepend) == 0 {
		return au
	}
	out := make([][]byte, 0, len(prepend)+len(au))
	out = append(out, prepend...)
	out = append(out, au...)
	return out
}

// annexBMarshal serialises an access unit (slice of raw NALs without start
// codes) into the Annex-B byte stream pion's H.264 payloader expects:
// each NAL prefixed with 0x00 0x00 0x00 0x01.
func annexBMarshal(au [][]byte) []byte {
	size := 0
	for _, nalu := range au {
		size += 4 + len(nalu)
	}
	buf := make([]byte, 0, size)
	for _, nalu := range au {
		buf = append(buf, 0x00, 0x00, 0x00, 0x01)
		buf = append(buf, nalu...)
	}
	return buf
}

// frameDuration returns the gap between the previous and current PTS as a
// time.Duration. The first frame (or a non-monotonic / suspiciously large
// gap caused by a stream hiccup) falls back to 33 ms, i.e. a 30 fps default.
func frameDuration(pts, prev int64, prevSet bool) time.Duration {
	var dur time.Duration
	if prevSet {
		if delta := pts - prev; delta > 0 {
			dur = time.Duration(delta) * time.Second / h264ClockRate
		}
	}
	if dur <= 0 || dur > 200*time.Millisecond {
		dur = 33 * time.Millisecond
	}
	return dur
}
