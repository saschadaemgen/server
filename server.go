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
	"carvilon.local/stream/internal/source"
)

//go:embed web
var webFS embed.FS

// h264ClockRate is the RTP clock rate for H.264 (90 kHz, RFC 6184).
const h264ClockRate = 90000

// Server is the CARVILON streaming kernel: a tiny WebRTC signaling
// endpoint, an embedded test page, and a frame-consumer that turns
// upstream access units into pion samples for any number of viewers
// (today: single-viewer spike).
//
// The kernel knows nothing about how frames are acquired — it accepts a
// [source.VideoSource] and only ever calls its interface methods. UniFi,
// generic RTSP, ESP32 etc. plug in through the same shape.
//
// Signaling protocol: POST the browser's offer SDP to /offer with
// Content-Type application/sdp, receive the answer SDP in the response
// body. No trickle ICE, no auth — spike scope only.
type Server struct {
	src    source.VideoSource
	addr   string
	logger *log.Logger

	api   *webrtc.API
	track *webrtc.TrackLocalStaticSample
	srv   *http.Server

	drops *droplog.Counter
}

// ServerOptions configures a [Server].
type ServerOptions struct {
	// Source is the video producer. Must already be Started.
	Source source.VideoSource

	// Addr is the HTTP listen address, e.g. ":8555". Avoid 9080
	// (carvilon-server) and 1984 (go2rtc).
	Addr string

	// Logger receives diagnostic output. If nil, the default logger.
	Logger *log.Logger
}

// NewServer builds a Server.
func NewServer(opts ServerOptions) (*Server, error) {
	if opts.Source == nil {
		return nil, errors.New("stream: Source is required")
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

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: h264ClockRate},
		"video",
		"carvilon-stream",
	)
	if err != nil {
		return nil, fmt.Errorf("stream: create track: %w", err)
	}

	s := &Server{
		src:    opts.Source,
		addr:   opts.Addr,
		logger: opts.Logger,
		api:    api,
		track:  track,
		drops:  &droplog.Counter{Logger: opts.Logger, Label: "stream: writesample"},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/offer", s.handleOffer)
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

// Run starts the frame consumer (source → pion samples) and the HTTP
// signaling server. It blocks until ctx is cancelled or the HTTP server
// errors out. On clean shutdown (ctx cancelled) it returns ctx.Err().
func (s *Server) Run(ctx context.Context) error {
	go s.consumeFrames(ctx)

	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
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
		return ctx.Err()
	case err := <-serveDone:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// consumeFrames pulls AUs from the source, turns each into a pion
// media.Sample (Annex-B body + duration from PTS delta), and writes it
// to the shared video track. Drops on write error are counted into the
// shared rate-limited logger; the loop never dies on a single sample.
func (s *Server) consumeFrames(ctx context.Context) {
	var (
		prevPTS    int64
		prevPTSSet bool
	)

	for {
		select {
		case <-ctx.Done():
			return
		case au, ok := <-s.src.Frames():
			if !ok {
				s.logger.Printf("stream: source frames channel closed; consumer exiting")
				return
			}

			dur := frameDuration(au.PTS, prevPTS, prevPTSSet)
			prevPTS = au.PTS
			prevPTSSet = true

			if err := s.track.WriteSample(media.Sample{
				Data:     annexBMarshal(au.NALUs),
				Duration: dur,
			}); err != nil {
				s.drops.Record(err)
			}
		}
	}
}

// handleOffer implements POST /offer: parse the browser's SDP offer,
// attach the shared track to a fresh PeerConnection, gather ICE, return
// the answer SDP.
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

	pc, err := s.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		http.Error(w, "create peer: "+err.Error(), http.StatusInternalServerError)
		return
	}

	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		s.logger.Printf("stream: peer %p state=%s", pc, st)
		switch st {
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed,
			webrtc.PeerConnectionStateDisconnected:
			_ = pc.Close()
		}
	})

	if _, err := pc.AddTrack(s.track); err != nil {
		_ = pc.Close()
		http.Error(w, "add track: "+err.Error(), http.StatusInternalServerError)
		return
	}

	offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(body)}
	if err := pc.SetRemoteDescription(offer); err != nil {
		_ = pc.Close()
		http.Error(w, "set remote: "+err.Error(), http.StatusBadRequest)
		return
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		_ = pc.Close()
		http.Error(w, "create answer: "+err.Error(), http.StatusInternalServerError)
		return
	}

	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		_ = pc.Close()
		http.Error(w, "set local: "+err.Error(), http.StatusInternalServerError)
		return
	}
	<-gathered

	w.Header().Set("Content-Type", "application/sdp")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, pc.LocalDescription().SDP)
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
