package stream

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"carvilon.local/stream/internal/droplog"
	"carvilon.local/stream/internal/hub"
	"carvilon.local/stream/internal/mjpeg"
)

//go:embed web
var webFS embed.FS

// h264ClockRate is the RTP clock rate for H.264 (90 kHz, RFC 6184).
const h264ClockRate = 90000

// Server is the CARVILON streaming kernel. It hosts a small WebRTC
// signaling endpoint plus an embedded test page, and fans out a single
// upstream source pull to N concurrent browser viewers.
//
// Each /offer request becomes its own [hub.Subscriber] with its own
// [webrtc.TrackLocalStaticSample] and its own feeder goroutine. A slow
// viewer can only ever delay itself — the source pull and the other
// viewers are isolated through the hub's non-blocking distribution.
//
// Source lifecycle is managed by the hub: the first connecting viewer
// triggers a fresh source build via [hub.SourceFactory] and Start; when
// the last viewer leaves, the source is Closed. The next connection
// rebuilds. This keeps the camera idle when no one is watching.
type Server struct {
	hub      *hub.Hub
	mjpegHub *mjpeg.Hub
	addr     string
	logger   *log.Logger

	api *webrtc.API
	srv *http.Server
}

// ServerOptions configures a [Server].
type ServerOptions struct {
	// SourceFactory builds a fresh, un-Started [source.VideoSource] on
	// demand. The hub invokes it at first Subscribe and again after the
	// subscriber count drops back to zero. The factory is invoked
	// without arguments — capture whatever configuration the source
	// needs in a closure (see cmd/spike/main.go for the pattern).
	SourceFactory hub.SourceFactory

	// Addr is the HTTP listen address, e.g. ":8555". Avoid 9080
	// (carvilon-server) and 1984 (go2rtc).
	Addr string

	// Logger receives diagnostic output. If nil, the default logger.
	Logger *log.Logger

	// SubscriberBuffer is forwarded to [hub.Options.SubscriberBuffer].
	// Zero means hub default (30).
	SubscriberBuffer int

	// MJPEGProfiles registers MJPEG output profiles. If empty, the
	// /api/stream.mjpeg endpoint returns 503 for every request — the
	// server still serves WebRTC viewers fine, only MJPEG is disabled.
	MJPEGProfiles []mjpeg.Profile

	// FFmpegPath is the path to the ffmpeg binary used by the MJPEG
	// encoder. Defaults to "ffmpeg" (resolved via $PATH). Ignored if
	// MJPEGProfiles is empty.
	FFmpegPath string
}

// NewServer builds a Server. The underlying hub is started here and
// torn down by [Server.Run]'s shutdown path.
func NewServer(opts ServerOptions) (*Server, error) {
	if opts.SourceFactory == nil {
		return nil, errors.New("stream: SourceFactory is required")
	}
	if opts.Addr == "" {
		return nil, errors.New("stream: Addr is required")
	}
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}

	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		return nil, fmt.Errorf("stream: register codecs: %w", err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me))

	h := hub.New(opts.SourceFactory, hub.Options{
		Logger:           opts.Logger,
		SubscriberBuffer: opts.SubscriberBuffer,
	})

	var mh *mjpeg.Hub
	if len(opts.MJPEGProfiles) > 0 {
		var err error
		mh, err = mjpeg.NewHub(mjpeg.HubOptions{
			StreamHub:  h,
			Profiles:   opts.MJPEGProfiles,
			FFmpegPath: opts.FFmpegPath,
			Logger:     opts.Logger,
		})
		if err != nil {
			_ = h.Close()
			return nil, fmt.Errorf("stream: mjpeg hub: %w", err)
		}
	}

	s := &Server{
		hub:      h,
		mjpegHub: mh,
		addr:     opts.Addr,
		logger:   opts.Logger,
		api:      api,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/offer", s.handleOffer)
	mux.HandleFunc("/api/stream.mjpeg", s.handleMJPEG)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		return nil, fmt.Errorf("stream: web embed: %w", err)
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	s.srv = &http.Server{
		Addr:              opts.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s, nil
}

// Run starts the HTTP signaling server and blocks until ctx is cancelled
// or the server errors out. On clean shutdown (ctx cancelled) it tears
// the hub down (closing source and all subscribers) and returns
// ctx.Err().
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		_ = s.hub.Close()
		return fmt.Errorf("stream: listen %s: %w", s.addr, err)
	}
	s.logger.Printf("stream: signaling + test page on http://%s", ln.Addr())

	serveDone := make(chan error, 1)
	go func() { serveDone <- s.srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutdownCtx)
		if s.mjpegHub != nil {
			_ = s.mjpegHub.Close()
		}
		_ = s.hub.Close()
		return ctx.Err()
	case err := <-serveDone:
		if s.mjpegHub != nil {
			_ = s.mjpegHub.Close()
		}
		_ = s.hub.Close()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// handleOffer implements POST /offer:
//   - Subscribes a new viewer at the hub (may start the source on the
//     very first viewer; that step blocks for the RTSP/Protect-API
//     bring-up, typically 1–3 s).
//   - Builds a per-viewer [webrtc.TrackLocalStaticSample] H.264 track.
//   - Spawns a feeder goroutine that drains the subscriber's frames
//     into the track.
//   - Performs the standard WebRTC offer/answer exchange.
//
// On peer-connection close/fail/disconnect the subscriber is removed
// and, if it was the last one, the hub will close the source.
func (s *Server) handleOffer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read offer: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) == 0 {
		http.Error(w, "empty offer", http.StatusBadRequest)
		return
	}

	sub, err := s.hub.Subscribe()
	if err != nil {
		http.Error(w, "subscribe: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: h264ClockRate},
		fmt.Sprintf("video-%d", sub.ID()),
		fmt.Sprintf("carvilon-viewer-%d", sub.ID()),
	)
	if err != nil {
		sub.Close()
		http.Error(w, "create track: "+err.Error(), http.StatusInternalServerError)
		return
	}

	pc, err := s.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		sub.Close()
		http.Error(w, "create peer: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Wire down-edge cleanup: any non-success terminal state on the
	// PC closes our subscriber. The hub then cleans up the channel
	// and (if this was the last viewer) stops the source.
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		s.logger.Printf("stream: viewer %d state=%s", sub.ID(), st)
		switch st {
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed,
			webrtc.PeerConnectionStateDisconnected:
			sub.Close()
			_ = pc.Close()
		}
	})

	if _, err := pc.AddTrack(track); err != nil {
		sub.Close()
		_ = pc.Close()
		http.Error(w, "add track: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Feeder goroutine: drains the subscriber's frames into the track.
	// Exits when the subscriber channel closes (subscriber Close, hub
	// shutdown, source end). On exit we close the PC defensively so a
	// source-side failure tears the viewer connection down too.
	feedDrops := &droplog.Counter{
		Logger: s.logger,
		Label:  fmt.Sprintf("stream: viewer %d writesample", sub.ID()),
	}
	go func() {
		defer func() { _ = pc.Close() }()
		s.feedTrack(sub, track, feedDrops)
	}()

	offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(body)}
	if err := pc.SetRemoteDescription(offer); err != nil {
		sub.Close()
		_ = pc.Close()
		http.Error(w, "set remote: "+err.Error(), http.StatusBadRequest)
		return
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		sub.Close()
		_ = pc.Close()
		http.Error(w, "create answer: "+err.Error(), http.StatusInternalServerError)
		return
	}

	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		sub.Close()
		_ = pc.Close()
		http.Error(w, "set local: "+err.Error(), http.StatusInternalServerError)
		return
	}
	<-gathered

	w.Header().Set("Content-Type", "application/sdp")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, pc.LocalDescription().SDP)
}

// feedTrack pumps access units from a subscriber into a pion sample
// track. Per-viewer PTS tracking lives here — pion's WriteSample wants
// a delta (Duration) per sample, not absolute timestamps.
func (s *Server) feedTrack(sub *hub.Subscriber, track *webrtc.TrackLocalStaticSample, drops *droplog.Counter) {
	var (
		prevPTS    int64
		prevPTSSet bool
	)
	for au := range sub.Frames() {
		dur := frameDuration(au.PTS, prevPTS, prevPTSSet)
		prevPTS = au.PTS
		prevPTSSet = true

		if err := track.WriteSample(media.Sample{
			Data:     annexBMarshal(au.NALUs),
			Duration: dur,
		}); err != nil {
			drops.Record(err)
		}
	}
}

// handleMJPEG implements GET /api/stream.mjpeg?src=<profile>.
//
// The endpoint is intentionally drop-in compatible with go2rtc — the
// carvilon-proxy forwards bytes verbatim, so the wire format must match
// to the byte. See [mjpeg.HeaderContentType] / [mjpeg.FramePrefix] for
// the locked-in layout.
//
// The handler keeps a single [mjpeg.Subscriber] for the duration of the
// HTTP response; closing the response (client disconnect or server
// shutdown) closes the subscriber, which triggers the per-profile
// session's last-viewer teardown if appropriate.
func (s *Server) handleMJPEG(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.mjpegHub == nil {
		http.Error(w, "mjpeg not enabled", http.StatusServiceUnavailable)
		return
	}

	profile := r.URL.Query().Get("src")
	if profile == "" {
		http.Error(w, "missing src parameter", http.StatusBadRequest)
		return
	}

	sub, err := s.mjpegHub.Subscribe(profile)
	if err != nil {
		if errors.Is(err, mjpeg.ErrUnknownProfile) {
			http.Error(w, "unknown profile", http.StatusNotFound)
			return
		}
		http.Error(w, "subscribe: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer sub.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	mjpeg.SetResponseHeaders(w)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	if r.Method == http.MethodHead {
		return
	}

	writer := mjpeg.NewWriter(w)
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-sub.Frames():
			if !ok {
				// Session ended (encoder died, server shutting down, …).
				return
			}
			if _, err := writer.Write(frame); err != nil {
				// Client gone, or proxy upstream closed. Either way we
				// just exit; defer sub.Close() handles cleanup.
				return
			}
			flusher.Flush()
		}
	}
}

// --- helpers ---------------------------------------------------------------

// annexBMarshal serialises a slice of raw NALs (no start codes) into the
// Annex-B byte stream pion's H.264 payloader expects: each NAL prefixed
// with 0x00 0x00 0x00 0x01.
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
// time.Duration. Sample.Duration tells pion how much to advance the
// outbound RTP timestamp before the NEXT sample — for constant frame rate
// this is exact, for variable rate it is off by at most one frame. The
// first frame and any suspicious gap (zero / negative / > 200 ms) falls
// back to 33 ms (~30 fps).
//
// When a new viewer joins, the hub pre-feeds the cached IDR before any
// fresh AUs. That cached IDR has the old camera-side PTS, so the first
// subsequent fresh AU produces a "huge gap" that this safety cap quietly
// clamps to 33 ms. Net effect: the per-viewer RTP stream stays monotonic
// with reasonable frame spacing from the very first sample. The fact that
// the absolute pion timestamps differ from the camera's PTS is fine —
// only relative spacing matters to the browser decoder.
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

