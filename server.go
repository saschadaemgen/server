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
	"carvilon.local/stream/internal/profile"
	"carvilon.local/stream/internal/source"
	"carvilon.local/stream/internal/sourcereg"
)

//go:embed web
var webFS embed.FS

// h264ClockRate is the RTP clock rate for H.264 (90 kHz, RFC 6184).
const h264ClockRate = 90000

// Server is the CARVILON streaming kernel: HTTP signaling, multi-camera
// fan-out, MJPEG transcoding.
//
// All viewer endpoints are profile-driven (?src=<name>):
//
//   - POST /offer?src=<browser-profile>      → WebRTC viewer for that
//     profile's camera (Usage=browser).
//   - GET /api/stream.mjpeg?src=<esp-profile> → MJPEG stream for that
//     profile's camera (Usage=esp).
//
// Source lifecycle is bedarfsgesteuert (S4-01): a camera is pulled
// ONLY when at least one viewer of any usage is currently watching it.
// 0 viewers across all profiles for a (camera, quality) ⇒ no pull, no
// decode, no ffmpeg.
type Server struct {
	profiles *profile.Registry
	sources  *sourcereg.Registry
	mjpegHub *mjpeg.Hub
	addr     string
	logger   *log.Logger

	api *webrtc.API
	srv *http.Server
}

// ServerOptions configures a [Server].
type ServerOptions struct {
	// Profiles is the registry of all named profiles, of any usage.
	// Required. The server filters by usage at the relevant endpoint.
	Profiles *profile.Registry

	// SourceFactory builds a fresh, un-Started [source.VideoSource] for
	// a given (CameraID, Quality) on demand. Invoked lazily by the
	// source registry: nothing happens until a viewer arrives for that
	// key.
	SourceFactory sourcereg.Factory

	// Addr is the HTTP listen address, e.g. ":8555". Avoid 9080
	// (carvilon-server) and 1984 (go2rtc).
	Addr string

	// Logger receives diagnostic output. If nil, the default logger.
	Logger *log.Logger

	// SubscriberBuffer is forwarded to the per-camera hub.Options.
	// Zero means hub default (30).
	SubscriberBuffer int

	// FFmpegPath is the path to the ffmpeg binary used by the MJPEG
	// encoders. Defaults to "ffmpeg" (resolved via $PATH).
	FFmpegPath string

	// EnableMJPEG controls whether the /api/stream.mjpeg endpoint is
	// active. Defaults to true. Set to false for WebRTC-only dev runs
	// on machines without ffmpeg.
	EnableMJPEG bool
}

// NewServer builds a Server. Hubs and the source registry start here
// and are torn down by [Server.Run]'s shutdown path.
func NewServer(opts ServerOptions) (*Server, error) {
	if opts.Profiles == nil {
		return nil, errors.New("stream: Profiles registry is required")
	}
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

	srcReg := sourcereg.New(opts.SourceFactory, opts.Logger)

	s := &Server{
		profiles: opts.Profiles,
		sources:  srcReg,
		addr:     opts.Addr,
		logger:   opts.Logger,
		api:      api,
	}

	if opts.EnableMJPEG {
		mh, err := mjpeg.NewHub(mjpeg.HubOptions{
			EntryFor:         s.mjpegEntryFor,
			FFmpegPath:       opts.FFmpegPath,
			Logger:           opts.Logger,
			SubscriberBuffer: opts.SubscriberBuffer,
		})
		if err != nil {
			_ = srcReg.Close()
			return nil, fmt.Errorf("stream: mjpeg hub: %w", err)
		}
		s.mjpegHub = mh
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/offer", s.handleOffer)
	mux.HandleFunc("/api/stream.mjpeg", s.handleMJPEG)
	mux.HandleFunc("/api/profiles", s.handleProfiles)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		_ = srcReg.Close()
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

// mjpegEntryFor implements the resolver the [mjpeg.Hub] needs: turn a
// profile name into an Entry (encode spec + source hub).
//
//   - Unknown name → propagate [profile.ErrUnknownProfile] for 404.
//   - Wrong codec (not "mjpeg") → explicit error so the caller learns
//     they hit the wrong endpoint. S6-01 changed the gate from
//     usage=esp to codec=mjpeg: /offer handles h264_passthrough and a
//     future /stream/h264 endpoint handles h264_cbp. Same profile
//     registry, three different output endpoints.
//   - Otherwise: derive the encode spec from the profile's own
//     Width/Height/FPS/EncodeQuality (S6-01 SpecFromProfile path) and
//     resolve the camera-side hub via the source registry. The source
//     hub for a given (CameraID, Quality) is shared by ALL MJPEG
//     profiles AND by /offer subscribers — a single upstream pull
//     serves everyone.
func (s *Server) mjpegEntryFor(name string) (mjpeg.Entry, error) {
	p, err := s.profiles.Get(name)
	if err != nil {
		return mjpeg.Entry{}, err
	}
	if p.Codec != profile.CodecMJPEG {
		return mjpeg.Entry{}, fmt.Errorf("profile %q has codec=%q, not %q (use the matching endpoint: /offer for h264_passthrough, /stream/h264 for h264_cbp)",
			p.Name, p.Codec, profile.CodecMJPEG)
	}
	spec, err := mjpeg.SpecFromProfile(p)
	if err != nil {
		return mjpeg.Entry{}, err
	}
	srcHub := s.sources.HubFor(sourcereg.Key{CameraID: p.CameraID, Quality: string(p.Quality)})
	return mjpeg.Entry{Spec: spec, Source: &hubAdapter{h: srcHub}}, nil
}

// Run starts the HTTP signaling server and blocks until ctx is cancelled
// or the server errors out. On clean shutdown (ctx cancelled) it tears
// the hub + source registry down and returns ctx.Err().
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		s.shutdownAll()
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
		s.shutdownAll()
		return ctx.Err()
	case err := <-serveDone:
		s.shutdownAll()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// shutdownAll closes the MJPEG hub (if present) before the source
// registry, so per-camera hubs receive last-unsubscribe signals before
// being torn down.
func (s *Server) shutdownAll() {
	if s.mjpegHub != nil {
		_ = s.mjpegHub.Close()
	}
	_ = s.sources.Close()
}

// handleOffer implements POST /offer?src=<browser-profile>:
//   - Looks the profile up and checks Usage=browser.
//   - Resolves the camera-side hub via [sourcereg.Registry.HubFor].
//   - Subscribes a new viewer; the camera-pull starts here only if
//     this is the first subscriber for that (CameraID, Quality).
//   - Builds a per-viewer [webrtc.TrackLocalStaticSample] H.264 track.
//   - Performs the standard WebRTC offer/answer exchange.
//
// On peer-connection close/fail/disconnect the subscriber is removed
// and, if it was the last one on the camera, the pull stops.
func (s *Server) handleOffer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	profName := r.URL.Query().Get("src")
	if profName == "" {
		http.Error(w, "missing src parameter", http.StatusBadRequest)
		return
	}
	p, err := s.profiles.Get(profName)
	if err != nil {
		if errors.Is(err, profile.ErrUnknownProfile) {
			http.Error(w, "unknown profile", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// S6-01: gate /offer on Codec, not Usage. h264_passthrough is the
	// only codec /offer can serve — the WebRTC payload IS the camera's
	// H.264 stream, no transcode. mjpeg and h264_cbp have their own
	// endpoints (/api/stream.mjpeg and /stream/h264 respectively).
	if p.Codec != profile.CodecH264Passthrough {
		http.Error(w, fmt.Sprintf("profile %q has codec=%q, not %q (use /api/stream.mjpeg for mjpeg, /stream/h264 for h264_cbp)",
			p.Name, p.Codec, profile.CodecH264Passthrough), http.StatusBadRequest)
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

	srcHub := s.sources.HubFor(sourcereg.Key{CameraID: p.CameraID, Quality: string(p.Quality)})
	sub, err := srcHub.Subscribe()
	if err != nil {
		http.Error(w, "subscribe: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: h264ClockRate},
		fmt.Sprintf("video-%s-%d", p.Name, sub.ID()),
		fmt.Sprintf("carvilon-%s-%d", p.Name, sub.ID()),
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

	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		s.logger.Printf("stream: viewer %s/%d state=%s", p.Name, sub.ID(), st)
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

	feedDrops := &droplog.Counter{
		Logger: s.logger,
		Label:  fmt.Sprintf("stream: viewer %s/%d writesample", p.Name, sub.ID()),
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

// handleMJPEG implements GET /api/stream.mjpeg?src=<esp-profile>.
//
// The endpoint is intentionally drop-in compatible with go2rtc — the
// carvilon-proxy forwards bytes verbatim, so the wire format must match
// to the byte. See [mjpeg.HeaderContentType] / [mjpeg.FramePrefix] for
// the locked-in layout.
//
// The profile's CameraID determines which camera is pulled; multiple
// MJPEG profiles on the SAME camera share one upstream RTSP pull via
// the source registry.
func (s *Server) handleMJPEG(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.mjpegHub == nil {
		http.Error(w, "mjpeg not enabled", http.StatusServiceUnavailable)
		return
	}

	profName := r.URL.Query().Get("src")
	if profName == "" {
		http.Error(w, "missing src parameter", http.StatusBadRequest)
		return
	}

	sub, err := s.mjpegHub.Subscribe(profName)
	if err != nil {
		if errors.Is(err, profile.ErrUnknownProfile) {
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

// handleProfiles returns the registered profile names + their public
// fields as JSON. Useful for the admin UI and for the spike's web/
// test page to discover available streams without hardcoding.
func (s *Server) handleProfiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	all := s.profiles.All()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	// Hand-roll JSON to avoid pulling encoding/json into the mainline
	// hot path. The output is stable, ordered by profile name. S6-01
	// adds the codec quintet so the admin UI can show / edit the encode
	// parameters that drive the ESP measurement campaign.
	_, _ = io.WriteString(w, "[")
	for i, p := range all {
		if i > 0 {
			_, _ = io.WriteString(w, ",")
		}
		fmt.Fprintf(w,
			`{"name":%q,"cameraID":%q,"quality":%q,"usage":%q,"description":%q,`+
				`"codec":%q,"width":%d,"height":%d,"fps":%d,"encodeQuality":%d}`,
			p.Name, p.CameraID, string(p.Quality), string(p.Usage), p.Description,
			string(p.Codec), p.Width, p.Height, p.FPS, p.EncodeQuality,
		)
	}
	_, _ = io.WriteString(w, "]")
}

// --- helpers ---------------------------------------------------------------

// hubAdapter wraps *hub.Hub to satisfy [mjpeg.SourceHub]. The two-level
// indirection lets mjpeg.Hub be tested without spinning up a real H.264
// hub.
type hubAdapter struct{ h *hub.Hub }

func (a *hubAdapter) Subscribe() (mjpeg.SourceSubscriber, error) {
	sub, err := a.h.Subscribe()
	if err != nil {
		return nil, err
	}
	return &subAdapter{s: sub}, nil
}

type subAdapter struct{ s *hub.Subscriber }

func (a *subAdapter) Frames() <-chan source.AccessUnit { return a.s.Frames() }
func (a *subAdapter) Close()                           { a.s.Close() }

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
