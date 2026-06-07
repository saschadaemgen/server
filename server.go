package stream

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"carvilon.local/stream/internal/droplog"
	"carvilon.local/stream/internal/h264esp"
	"carvilon.local/stream/internal/hub"
	"carvilon.local/stream/internal/mjpeg"
	"carvilon.local/stream/internal/proccpu"
	"carvilon.local/stream/internal/profile"
	"carvilon.local/stream/internal/source"
	"carvilon.local/stream/internal/sourcereg"
	"carvilon.local/stream/internal/stats"
)

//go:embed web
var webFS embed.FS

// h264ClockRate is the RTP clock rate for H.264 (90 kHz, RFC 6184).
const h264ClockRate = 90000

// Server is the CARVILON streaming kernel: HTTP signaling, multi-camera
// fan-out, MJPEG + H.264-CBP transcoding.
//
// All viewer endpoints are profile-driven (?src=<name>); the codec on
// the chosen profile determines which endpoint serves it:
//
//   - POST /offer?src=<profile>            → WebRTC viewer for
//     codec=h264_passthrough profiles (the camera's H.264 shipped
//     verbatim through DTLS-SRTP).
//   - GET /api/stream.mjpeg?src=<profile>  → multipart MJPEG for
//     codec=mjpeg profiles (ffmpeg transcodes to JPEG, the bytes
//     match go2rtc 1:1).
//   - GET /stream/h264?src=<profile>       → chunked Annex-B for
//     codec=h264_cbp profiles (ffmpeg transcodes to Constrained
//     Baseline; SPS/PPS in-band before every IDR; ONE Access Unit
//     per HTTP chunk — the S6-02 wire shape).
//
// Source lifecycle is bedarfsgesteuert (S4-01): a camera is pulled
// ONLY when at least one viewer of any usage is currently watching it.
// 0 viewers across all profiles for a (camera, quality) ⇒ no pull, no
// decode, no ffmpeg. Two profiles sharing the same (CameraID, Quality)
// share a single upstream camera pull AND a single transcoder per
// codec.
type Server struct {
	profiles   *profile.Registry
	sources    *sourcereg.Registry
	mjpegHub   *mjpeg.Hub
	h264espHub *h264esp.Hub // S6-02: H.264 CBP transcode -> ESP via /stream/h264
	addr       string
	logger     *log.Logger

	// S6-01 measurement apparatus. Both nil-safe: if unset, the
	// /stream/stats endpoint returns zeros and the periodic log is
	// silenced.
	stats    *stats.Registry
	cpu      *proccpu.Sampler
	statsLog time.Duration // 0 = no periodic log

	// S6-01 tuning surface. nil → PUT/DELETE /api/profiles/{name}
	// return 503; the read-only GET still works.
	writer ProfileWriter

	// S6-14: global camera-side wire-protection mode. Canonical
	// ("tls" or "srtp", never empty) after NewServer. Used by
	// sourceKeyFor to build the per-pull Key and by handleProfiles
	// to surface the live mode in the GET output.
	encryption string

	api *webrtc.API
	srv *http.Server

	// S4 LAN-direct WHEP (edge_whep.go). whepAPI is built only when
	// opts.LANWHEPICEPort > 0 (a separate webrtc.API with a fixed-UDP-port
	// SettingEngine advertising the real LAN host candidate); the POST
	// /whep/{streamID} route is registered only then. egressKey, when set,
	// gates that endpoint with a Bearer egress_token (fail-closed); empty ->
	// open like /offer on the LAN.
	whepAPI   *webrtc.API
	whepMux   ice.UDPMux // the fixed-port LAN ICE mux; closed on shutdown to release the UDP port
	egressKey []byte
}

// ServerOptions configures a [Server].
type ServerOptions struct {
	// Profiles is the registry of all named profiles, of any usage.
	// Required. The server filters by usage at the relevant endpoint.
	Profiles *profile.Registry

	// SourceFactory builds a fresh, un-Started [source.VideoSource] for
	// a given (CameraID, Quality) on demand. Invoked lazily by the
	// source registry: nothing happens until a viewer arrives for that
	// key. Required UNLESS Sources is supplied (the registry already
	// embeds a factory in that case).
	SourceFactory sourcereg.Factory

	// Sources (optional, S2-06) is a pre-built shared source registry.
	// When non-nil it is used verbatim and SourceFactory is ignored;
	// when nil, NewServer builds its own registry from SourceFactory
	// (the standalone cmd/streaming-server path, unchanged).
	//
	// Passing a shared registry is what lets a *Server (TrackForStream /
	// the local WebRTC+MJPEG path) and a [streambackend.Backend] feed
	// off ONE camera pull instead of two — see [SetupEdgeInProcess].
	// The owner of the shared registry is responsible for closing it;
	// [sourcereg.Registry.Close] is idempotent, so the server's own
	// shutdown path closing it again is harmless.
	Sources *sourcereg.Registry

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

	// Stats (optional) is the per-client throughput tracker that
	// powers /stream/stats. If nil, /stream/stats reports an empty
	// snapshot and the per-frame instrumentation in handleMJPEG is a
	// no-op — the same code path runs, so the production binary's
	// behaviour matches a Stats-less unit-test setup byte-for-byte.
	Stats *stats.Registry

	// CPU (optional) supplies the global.transcoder_cpu_percent field.
	// On non-Linux builds this is the stub Sampler that always returns
	// (0, false), so the field stays out of the JSON.
	CPU *proccpu.Sampler

	// StatsLogInterval, if non-zero, makes Run() spawn a goroutine
	// that prints a human-readable line every interval. 0 (default)
	// silences the periodic log. /stream/stats remains available
	// regardless.
	StatsLogInterval time.Duration

	// ProfileWriter (optional) enables runtime profile mutations via
	// PUT/DELETE /api/profiles/{name}. The intended implementation is
	// [streambackend.Backend], which writes the SQLite store and the
	// in-memory registry atomically. If nil, both endpoints return 503
	// — the read-only GET /api/profiles stays available so an admin
	// UI can still inspect the registered profiles.
	//
	// Live-session semantics: a PUT only affects new viewers; existing
	// viewers stay on the hub they joined. This is the spike-stage
	// rule documented in streambackend.Backend.Put.
	ProfileWriter ProfileWriter

	// Encryption is the GLOBAL wire-protection mode for the camera-side
	// (Protect → streaming-server) hop. Applied to every camera pull
	// regardless of which profile triggered it.
	//
	// S6-14: this replaces the per-profile encryption steering that
	// S6-12 introduced. Camera-side encryption is a property of the
	// SOURCE, not of the delivery profile; mixing it per profile led
	// to a bug where env=srtp was ignored because every DB profile
	// canonicalised to "tls" and silently won the priority.
	//
	// The per-profile [profile.Profile.Encryption] field is kept in
	// the schema (PUT body still accepts it; store still persists it;
	// GET still exposes it) but is now display-only — it is OVERWRITTEN
	// in the GET output by this server-global value, so the admin sees
	// what's actually running, not what was stored.
	//
	// Empty value (default) is treated as [profile.EncryptionTLS].
	// cmd/streaming-server feeds this from the UNIFI_ENCRYPTION env var.
	Encryption profile.Encryption

	// LANWHEPICEPort enables the S4 LAN-direct WHEP endpoint (POST
	// /whep/{streamID} on this same HTTP server) when > 0. The value is the
	// FIXED UDP port the edge binds for ICE media; the advertised host
	// candidate is <lan-ip>:<port>, so a same-WLAN phone connects directly
	// to the edge instead of via the VPS relay. 0 (default) -> the endpoint
	// is OFF (the standalone dev server and the cloud role do not bind it).
	LANWHEPICEPort int
	// EgressHMACKey, when set, gates the LAN WHEP endpoint with a Bearer
	// egress_token (verified by the same publishtoken.Verify, fail-closed),
	// mirroring the cloud egress. Empty -> the endpoint is open like /offer
	// on the LAN. Only consulted when LANWHEPICEPort > 0.
	EgressHMACKey []byte
}

// ProfileWriter is the mutating half of the profile surface — the
// read half is the [profile.Registry] passed in via Options.Profiles.
// Implemented by [streambackend.Backend]'s internal-shape methods
// ([streambackend.Backend.PutProfile] / DeleteProfile); a test fake
// can satisfy it without spinning up a store.
type ProfileWriter interface {
	PutProfile(ctx context.Context, p profile.Profile) error
	DeleteProfile(ctx context.Context, name string) error
}

// NewServer builds a Server. Hubs and the source registry start here
// and are torn down by [Server.Run]'s shutdown path.
func NewServer(opts ServerOptions) (*Server, error) {
	if opts.Profiles == nil {
		return nil, errors.New("stream: Profiles registry is required")
	}
	// S2-06: either a ready Sources registry or a factory to build one.
	if opts.Sources == nil && opts.SourceFactory == nil {
		return nil, errors.New("stream: SourceFactory or Sources is required")
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

	// S2-06: reuse a shared registry if given (so Server + Backend share
	// one camera pull), else build our own from the factory.
	srcReg := opts.Sources
	if srcReg == nil {
		srcReg = sourcereg.New(opts.SourceFactory, opts.Logger)
	}

	// S6-14: canonicalise the global encryption mode once at construction.
	// Empty → "tls"; anything else is taken verbatim (Validate happens
	// at the unifi.NewSource boundary in the source factory).
	encryption := string(opts.Encryption)
	if encryption == "" {
		encryption = string(profile.EncryptionTLS)
	}

	s := &Server{
		profiles:   opts.Profiles,
		sources:    srcReg,
		addr:       opts.Addr,
		logger:     opts.Logger,
		api:        api,
		stats:      opts.Stats,
		cpu:        opts.CPU,
		statsLog:   opts.StatsLogInterval,
		writer:     opts.ProfileWriter,
		encryption: encryption,
	}

	if opts.EnableMJPEG {
		// S6-04: source-measurement callbacks. Both hubs share the
		// same stats.Registry; the closures box the optional stats
		// pointer so callers without a Stats config still get the
		// nil-safe path.
		onSourceAU := func(p string) {
			if s.stats != nil {
				s.stats.RecordSourceFrame(p)
			}
		}
		onSessionStart := func(p string) {
			if s.stats != nil {
				s.stats.ResetSourceCounter(p)
			}
		}

		mh, err := mjpeg.NewHub(mjpeg.HubOptions{
			EntryFor:         s.mjpegEntryFor,
			FFmpegPath:       opts.FFmpegPath,
			Logger:           opts.Logger,
			SubscriberBuffer: opts.SubscriberBuffer,
			OnSourceAU:       onSourceAU,
			OnSessionStart:   onSessionStart,
		})
		if err != nil {
			_ = srcReg.Close()
			return nil, fmt.Errorf("stream: mjpeg hub: %w", err)
		}
		s.mjpegHub = mh

		// The H.264 CBP path lives in the same "needs ffmpeg" gate as
		// MJPEG — same binary, same subprocess strategy. EnableMJPEG
		// stays the single switch for "this build has ffmpeg
		// available"; a future EnableESPH264 split is easy if ffmpeg-
		// free deployments matter, but the spike doesn't need it.
		hh, err := h264esp.NewHub(h264esp.HubOptions{
			EntryFor:         s.h264espEntryFor,
			FFmpegPath:       opts.FFmpegPath,
			Logger:           opts.Logger,
			SubscriberBuffer: opts.SubscriberBuffer,
			OnSourceAU:       onSourceAU,
			OnSessionStart:   onSessionStart,
		})
		if err != nil {
			_ = mh.Close()
			_ = srcReg.Close()
			return nil, fmt.Errorf("stream: h264esp hub: %w", err)
		}
		s.h264espHub = hh
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/offer", s.handleOffer)
	mux.HandleFunc("/api/stream.mjpeg", s.handleMJPEG)
	mux.HandleFunc("/stream/h264", s.handleH264)
	mux.HandleFunc("/api/profiles", s.handleProfiles)
	// S6-01 tuning endpoints (Go 1.22+ method+path patterns). The
	// catch-all `/api/profiles` above keeps serving GET; these add
	// per-name PUT/DELETE without re-implementing routing.
	mux.HandleFunc("PUT /api/profiles/{name}", s.handleProfilePut)
	mux.HandleFunc("DELETE /api/profiles/{name}", s.handleProfileDelete)
	mux.HandleFunc("/stream/stats", s.handleStats)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	// S4 LAN-direct WHEP: a separate webrtc.API pinned to a fixed UDP port,
	// advertising the real LAN host candidate so a same-WLAN phone connects
	// directly to the edge (host pref 126 > VPS srflx 100). OFF by default
	// (port 0): the standalone dev server and the cloud role never bind it.
	if opts.LANWHEPICEPort > 0 {
		whepAPI, whepMux, werr := newLANWHEPAPI(opts.LANWHEPICEPort)
		if werr != nil {
			_ = srcReg.Close()
			return nil, fmt.Errorf("stream: lan whep: %w", werr)
		}
		s.whepAPI = whepAPI
		s.whepMux = whepMux
		s.egressKey = opts.EgressHMACKey
		mux.HandleFunc("POST /whep/{streamID}", s.handleEdgeWHEP)
		auth := "OFF (open, no key)"
		if len(s.egressKey) > 0 {
			auth = "ON (egress_token)"
		}
		opts.Logger.Printf("stream: LAN-direct WHEP on /whep, ICE media UDP :%d, egress auth %s", opts.LANWHEPICEPort, auth)
	}

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

// sourceKeyFor builds the per-pull key for a given profile.
//
// S6-14: the Encryption component of the key now comes from the
// SERVER-GLOBAL setting, NOT from the profile. This restores the
// intended primacy of the UNIFI_ENCRYPTION env var that S6-12
// accidentally broke (every DB row canonicalised to "tls" and won
// the priority). All profiles for the same (camera, quality) share
// a single hub again — the way Fan-Out was always supposed to work.
//
// Encryption stays in the Key because if a future change ever flips
// the global mode mid-life, an existing hub with the old mode would
// remain in the map distinct from the new one — clean separation,
// no silent inheritance.
func (s *Server) sourceKeyFor(p profile.Profile) sourcereg.Key {
	return sourcereg.Key{
		CameraID:   p.CameraID,
		Quality:    string(p.Quality),
		Encryption: s.encryption,
		// S4: the source-side pipeline (passthrough vs short-GOP re-encode)
		// derives from the codec. The re-encode codec gets its OWN hub for
		// the same (camera, quality, encryption) — fed by the passthrough
		// hub, so still one camera pull.
		Pipeline: p.Codec.Pipeline(),
	}
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
	srcHub := s.sources.HubFor(s.sourceKeyFor(p))
	return mjpeg.Entry{Spec: spec, Source: &hubAdapter{h: srcHub}}, nil
}

// h264espEntryFor implements the resolver the [h264esp.Hub] needs:
// same idea as [Server.mjpegEntryFor] but for the codec=h264_cbp
// profiles served at /stream/h264. The H.264 hub shares the per-
// (camera, quality) upstream pull with MJPEG and /offer — only the
// transcode layer is separate.
func (s *Server) h264espEntryFor(name string) (h264esp.Entry, error) {
	p, err := s.profiles.Get(name)
	if err != nil {
		return h264esp.Entry{}, err
	}
	if p.Codec != profile.CodecH264CBP {
		return h264esp.Entry{}, fmt.Errorf("profile %q has codec=%q, not %q (use the matching endpoint: /offer for h264_passthrough, /api/stream.mjpeg for mjpeg)",
			p.Name, p.Codec, profile.CodecH264CBP)
	}
	spec, err := h264esp.SpecFromProfile(p)
	if err != nil {
		return h264esp.Entry{}, err
	}
	srcHub := s.sources.HubFor(s.sourceKeyFor(p))
	return h264esp.Entry{Spec: spec, Source: &h264espHubAdapter{h: srcHub}}, nil
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

	// S6-01: periodic stats logger. Starts only when StatsLogInterval
	// was configured AND a Stats registry is wired; otherwise it's a
	// no-op goroutine that returns immediately. The shared ctx makes
	// shutdown automatic.
	go s.runStatsLogger(ctx)

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

// shutdownAll closes the per-codec hubs (if present) before the
// source registry, so per-camera hubs receive last-unsubscribe
// signals before being torn down. Order: per-codec encoders first,
// then the upstream camera bus.
func (s *Server) shutdownAll() {
	if s.mjpegHub != nil {
		_ = s.mjpegHub.Close()
	}
	if s.h264espHub != nil {
		_ = s.h264espHub.Close()
	}
	if s.whepMux != nil {
		_ = s.whepMux.Close() // release the fixed LAN ICE UDP port
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

	srcHub := s.sources.HubFor(s.sourceKeyFor(p))
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
		// S6-15: bring WebRTC viewers into /stream/stats. Codec uses
		// the profile codec (h264_passthrough) — the same convention
		// handleMJPEG / handleH264 follow. Stays consistent so the
		// per-profile aggregation in stats.Snapshot doesn't split a
		// profile across two codec names.
		//
		// Registration happens at goroutine start (signalling has
		// produced a peer connection; the briefing's "successful
		// peer-connection setup" trigger is the SDP-answer write
		// already in flight at the call site). LIFO defers below
		// guarantee pc.Close runs BEFORE Unregister, so when the
		// snapshot disappears the wire is already gone.
		var sc *stats.Client
		if s.stats != nil {
			sc = s.stats.Register(p.Name, string(p.Codec), r.RemoteAddr)
			defer s.stats.Unregister(sc)
		}
		defer func() { _ = pc.Close() }()
		s.feedTrack(sub, track, feedDrops, sc)
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

// webrtcIdleTimeout is the defensive watchdog window in [feedTrack]:
// if no AU arrives from the camera hub for this long, the feed loop
// exits even though [hub.Subscriber.Frames] has not been closed. The
// deferred chain in handleOffer's goroutine then runs (Unregister
// from stats, pc.Close), so a viewer can never linger as a "ghost"
// in /stream/stats if pion's OnConnectionStateChange callback fails
// to fire on disconnect (S6-15 briefing pflicht).
//
// 30 s is a wide margin: even the slowest configured profile delivers
// at 10 fps (mjpeg_hq → 100 ms inter-frame), and the camera-side bus
// pushes 30 fps to /offer subscribers (h264_passthrough — no
// throttling). 30 s = 300 missing frames, well beyond any healthy
// transient.
//
// It's a var (not const) so tests can shrink it to a millisecond
// range. Not exported — pkg-internal only.
var webrtcIdleTimeout = 30 * time.Second

// feedTrack pumps access units from a subscriber into a pion sample
// track. Per-viewer PTS tracking lives here — pion's WriteSample wants
// a delta (Duration) per sample, not absolute timestamps.
//
// S6-15: sc is the per-viewer stats handle (nil-safe). On every
// successful WriteSample we record the sample's wire size; on a
// WriteSample error we record a drop so /stream/stats reflects the
// teardown reason. The function also enforces an idle timeout against
// [webrtcIdleTimeout]; on expiry the loop exits so the caller's
// deferred Unregister + pc.Close run even if the upstream went silent
// without pion noticing.
func (s *Server) feedTrack(sub *hub.Subscriber, track *webrtc.TrackLocalStaticSample, drops *droplog.Counter, sc *stats.Client) {
	var (
		prevPTS    int64
		prevPTSSet bool
	)
	timer := time.NewTimer(webrtcIdleTimeout)
	defer timer.Stop()
	for {
		select {
		case au, ok := <-sub.Frames():
			if !ok {
				return
			}
			// Reset the idle timer for the next frame. Drain the
			// channel first if Stop reports the timer already fired
			// but its event is still queued — the standard
			// time.Timer.Reset dance.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(webrtcIdleTimeout)

			dur := frameDuration(au.PTS, prevPTS, prevPTSSet)
			prevPTS = au.PTS
			prevPTSSet = true

			payload := annexBMarshal(au.NALUs)
			if err := track.WriteSample(media.Sample{
				Data:     payload,
				Duration: dur,
			}); err != nil {
				drops.Record(err)
				sc.RecordDrop()
				continue
			}
			sc.RecordFrame(len(payload))
		case <-timer.C:
			// No frame for webrtcIdleTimeout. Returning here lets
			// the caller's deferred Unregister + pc.Close run — the
			// stats entry will disappear from the next snapshot
			// instead of lingering as a ghost.
			drops.Record(fmt.Errorf("webrtc viewer idle for %s; tearing down", webrtcIdleTimeout))
			return
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

	// S6-01: register with the stats tracker so /stream/stats sees this
	// client. The Profile lookup we already did above gave us the
	// codec; reuse it so the tracker doesn't have to re-resolve. Nil
	// stats registry → RecordFrame is a no-op (Client method on nil),
	// so the production code path matches a stats-less unit test.
	var sc *stats.Client
	if s.stats != nil {
		p, _ := s.profiles.Get(profName)
		sc = s.stats.Register(profName, string(p.Codec), r.RemoteAddr)
		defer s.stats.Unregister(sc)
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
			n, err := writer.Write(frame)
			if err != nil {
				// Client gone, or proxy upstream closed. Count the
				// undelivered frame as a drop (so the snapshot shows
				// where the stream stopped) and exit.
				sc.RecordDrop()
				return
			}
			sc.RecordFrame(n)
			flusher.Flush()
		}
	}
}

// handleH264 implements GET /stream/h264?src=<h264_cbp-profile> — the
// S6-02 endpoint that ships the Constrained-Baseline transcode to the
// ESP32-P4. Wire shape (briefing-pinned):
//
//   - One complete H.264 Access Unit per HTTP response chunk. The
//     chunked transfer-encoding IS the framing; no multipart, no
//     custom AU header.
//   - Annex-B with 4-byte start codes; NO AUDs.
//   - SPS / PPS in-band before every IDR/keyframe (handled by the
//     ffmpeg -bsf:v dump_extra=freq=keyframe flag in h264esp.OutputArgs).
//
// Live-session semantics mirror handleMJPEG: bedarfsgesteuert via the
// h264esp.Hub, one transcoder per profile, drop-statt-buffer on a
// slow client.
func (s *Server) handleH264(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.h264espHub == nil {
		http.Error(w, "h264 esp not enabled", http.StatusServiceUnavailable)
		return
	}

	profName := r.URL.Query().Get("src")
	if profName == "" {
		http.Error(w, "missing src parameter", http.StatusBadRequest)
		return
	}

	sub, err := s.h264espHub.Subscribe(profName)
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

	// Content-Type: H.264 Annex-B byte stream. No standard MIME type
	// fits exactly — video/h264 is the closest in common use; the ESP
	// doesn't parse the header anyway, but a sensible value helps
	// curl / browser sniffing.
	w.Header().Set("Content-Type", "video/h264")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	// X-Stream-Format makes the briefing's wire-shape contract
	// machine-readable: "annex-b" framing, "one AU per chunk", "no AUDs".
	// Convenient for ESP-side sanity asserts in test harnesses.
	w.Header().Set("X-Stream-Format", "annex-b; framing=chunk-per-au; aud=stripped")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	if r.Method == http.MethodHead {
		return
	}

	// S6-02 telemetry: register with the stats tracker so the
	// transcoder-cost vs MJPEG comparison is visible in /stream/stats.
	var sc *stats.Client
	if s.stats != nil {
		p, _ := s.profiles.Get(profName)
		sc = s.stats.Register(profName, string(p.Codec), r.RemoteAddr)
		defer s.stats.Unregister(sc)
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case au, ok := <-sub.Frames():
			if !ok {
				// Encoder died or server shutting down.
				return
			}
			// One AU per HTTP chunk = one Write + one Flush. The
			// chunked transfer-encoding wraps each Write into its own
			// chunk so the briefing's "one chunk = one AU" rule holds.
			n, err := w.Write(au)
			if err != nil {
				sc.RecordDrop()
				return
			}
			sc.RecordFrame(n)
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
	// hot path. The output is stable, ordered by profile name.
	//
	// S6-09: field names are snake_case to match PUT /api/profiles/{name}'s
	// body shape exactly. A client can therefore take a single entry
	// from this list, modify a field, and PUT it back without renaming
	// any keys — round-trip is bit-compatible. See TestProfiles_GetPut_RoundTrip
	// for the live contract. Field set:
	//   name, camera_id, quality, usage, description,
	//   codec, width, height, fps, encode_quality.
	_, _ = io.WriteString(w, "[")
	for i, p := range all {
		if i > 0 {
			_, _ = io.WriteString(w, ",")
		}
		// S6-14: the encryption field shows the SERVER-GLOBAL mode
		// that's actually driving the camera pull, NOT the per-
		// profile stored value. Camera-side encryption is a source
		// property, not a delivery property — the admin sees what
		// is really running. PUT still accepts the field (schema
		// is stable) but it no longer steers anything.
		fmt.Fprintf(w,
			`{"name":%q,"camera_id":%q,"quality":%q,"usage":%q,"description":%q,`+
				`"codec":%q,"width":%d,"height":%d,"fps":%d,"encode_quality":%d,`+
				`"encryption":%q}`,
			p.Name, p.CameraID, string(p.Quality), string(p.Usage), p.Description,
			string(p.Codec), p.Width, p.Height, p.FPS, p.EncodeQuality,
			s.encryption,
		)
	}
	_, _ = io.WriteString(w, "]")
}

// handleProfilePut implements PUT /api/profiles/{name}: parse the JSON
// body into a profile.Profile, override Name with the URL path so the
// REST identity wins, validate, and hand it to the [ProfileWriter] for
// persistence + registry-sync.
//
// Body shape mirrors the profile.Profile JSON (snake_case): camera_id,
// quality, usage, description, codec, width, height, fps,
// encode_quality. The Name in the body is ignored if present —
// /api/profiles/{name} IS the name; double-specifying would invite
// "two sources of truth" bugs.
//
// Returns:
//   - 204 No Content on success.
//   - 400 if the JSON is malformed or profile.Validate fails.
//   - 503 if no ProfileWriter is wired (read-only deployment).
//
// Live-session semantics are documented on [ProfileWriter] /
// [streambackend.Backend.PutProfile]: existing viewers stay on the old
// hub identity, new viewers pick up the change.
func (s *Server) handleProfilePut(w http.ResponseWriter, r *http.Request) {
	if s.writer == nil {
		http.Error(w, "profile write disabled (no ProfileWriter configured)", http.StatusServiceUnavailable)
		return
	}
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "missing profile name", http.StatusBadRequest)
		return
	}

	var body profileJSON
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}

	p := profile.Profile{
		Name:          name, // URL wins; body.Name is ignored.
		CameraID:      body.CameraID,
		Quality:       profile.Quality(body.Quality),
		Usage:         profile.Usage(body.Usage),
		Description:   body.Description,
		Codec:         profile.Codec(body.Codec),
		Width:         body.Width,
		Height:        body.Height,
		FPS:           body.FPS,
		EncodeQuality: body.EncodeQuality,
		Encryption:    profile.Encryption(body.Encryption),
	}
	if err := p.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := s.writer.PutProfile(r.Context(), p); err != nil {
		http.Error(w, "put: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.logger.Printf("profile: tuned %q codec=%s %dx%d@%dfps q=%d",
		p.Name, p.Codec, p.Width, p.Height, p.FPS, p.EncodeQuality)
	w.WriteHeader(http.StatusNoContent)
}

// handleProfileDelete implements DELETE /api/profiles/{name}: remove
// the named profile from the persistent store + the in-memory
// registry. Existing viewers of that profile keep streaming (the hub
// stays alive until the last subscriber leaves) — only NEW ?src=
// requests for the deleted name will 404.
func (s *Server) handleProfileDelete(w http.ResponseWriter, r *http.Request) {
	if s.writer == nil {
		http.Error(w, "profile write disabled (no ProfileWriter configured)", http.StatusServiceUnavailable)
		return
	}
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "missing profile name", http.StatusBadRequest)
		return
	}
	err := s.writer.DeleteProfile(r.Context(), name)
	if err != nil {
		// We don't have a typed sentinel reachable from this package
		// (the writer is an interface), so fall back to a substring
		// sniff for the "not found" case. The streambackend wraps
		// ErrProfileNotFound which renders with %q on the name; the
		// admin UI just needs the 404 to render correctly.
		// We can't reach into streambackend for the typed
		// ErrProfileNotFound because importing streambackend from
		// package stream would create a cycle (streambackend itself
		// imports the internal profile package shared with this one).
		// A substring sniff against the rendered error message is good
		// enough for the 404 vs 500 split — the streambackend formats
		// not-found with "not found".
		msg := strings.ToLower(err.Error())
		if errors.Is(err, profile.ErrUnknownProfile) ||
			strings.Contains(msg, "not found") ||
			strings.Contains(msg, "unknown") {
			http.Error(w, "unknown profile", http.StatusNotFound)
			return
		}
		http.Error(w, "delete: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.logger.Printf("profile: deleted %q", name)
	w.WriteHeader(http.StatusNoContent)
}

// profileJSON mirrors the JSON tags the admin UI and the streambackend
// wire shape use (snake_case). Kept private to server.go — outside
// callers go through streambackend.Profile, which has the same field
// set but adds the runtime Consumers count.
//
// S6-09: `name` is now an accepted-but-ignored field so a client can
// take an entry from `GET /api/profiles` and send it back to
// `PUT /api/profiles/{name}` verbatim — the URL path stays
// authoritative for the identity, the body's `name` is tolerated
// for round-trip ergonomics. DisallowUnknownFields stays on so
// genuine typos in OTHER field names still surface as 400.
type profileJSON struct {
	Name          string `json:"name"` // accepted but ignored; URL wins
	CameraID      string `json:"camera_id"`
	Quality       string `json:"quality"`
	Usage         string `json:"usage"`
	Description   string `json:"description"`
	Codec         string `json:"codec"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	FPS           int    `json:"fps"`
	EncodeQuality int    `json:"encode_quality"`
	// S6-12: optional. "tls" / "srtp" / "" (= default tls). Validate
	// rejects other values with 400.
	Encryption string `json:"encryption"`
}

// handleStats implements GET /stream/stats — the JSON snapshot of the
// per-client and global throughput counters that drive the S6-01
// measurement campaign.
//
// Empty-Stats safe: if no stats.Registry is wired in, the response is a
// minimal `{global:{clients:0,...}, profiles:{}, clients:[]}` shape so
// admin UIs can poll the endpoint unconditionally.
//
// The endpoint is read-only and cheap (a slice of atomic Loads under a
// short read lock); we intentionally do NOT cache it — every GET sees
// a fresh sample so an admin manually refreshing the page can watch
// the bitrate react to a tuning PUT.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	snap := s.collectStats()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(snap); err != nil {
		// The header is already out; just log and move on. The admin
		// UI will retry on the next poll.
		s.logger.Printf("stream: stats encode: %v", err)
	}
}

// collectStats builds the [stats.Snapshot] for /stream/stats AND for
// the periodic logger. Pulling it into its own helper keeps the two
// call sites in sync (same shape, same CPU sampling logic).
func (s *Server) collectStats() stats.Snapshot {
	var snap stats.Snapshot
	if s.stats != nil {
		snap = s.stats.Snapshot()
	} else {
		snap = stats.Snapshot{
			GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
			Profiles:    map[string]stats.ProfileSnapshot{},
			Clients:     []stats.ClientSnapshot{},
		}
	}
	if s.cpu != nil {
		if pct, ok := s.cpu.Sample(); ok {
			snap.Global.TranscoderCPUPercent = pct
		}
	}
	return snap
}

// runStatsLogger emits a human-readable summary line every
// s.statsLog interval. Off when statsLog == 0. The goroutine
// terminates when ctx is cancelled (Server.Run's shutdown path).
func (s *Server) runStatsLogger(ctx context.Context) {
	if s.statsLog <= 0 {
		return
	}
	t := time.NewTicker(s.statsLog)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			snap := s.collectStats()
			// One concise summary line, plus one indented line per
			// profile that currently has viewers. Empty system → one
			// line, easy to grep.
			cpuPart := ""
			if snap.Global.TranscoderCPUPercent > 0 {
				cpuPart = fmt.Sprintf(" cpu=%.1f%%", snap.Global.TranscoderCPUPercent)
			}
			s.logger.Printf("stats: clients=%d frames=%d bytes=%d%s",
				snap.Global.Clients,
				snap.Global.FramesSentTotal,
				snap.Global.BytesSentTotal,
				cpuPart,
			)
			for name, ps := range snap.Profiles {
				// S6-04: show source_fps alongside output fps so the
				// log line tells you immediately where rate is lost.
				srcPart := ""
				if ps.SourceFPS > 0 {
					srcPart = fmt.Sprintf(" src=%.1ffps", ps.SourceFPS)
				}
				s.logger.Printf("stats:   profile=%s codec=%s viewers=%d fps=%.1f kbps=%.1f bytes=%d%s",
					name, ps.Codec, ps.Clients, ps.AvgFPS, ps.AvgBitrateKbps, ps.BytesSent, srcPart)
			}
		}
	}
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

// h264espHubAdapter is the same shape as hubAdapter but typed against
// h264esp.SourceHub / SourceSubscriber. The two interfaces are
// structurally identical; we keep them per-package so the h264esp
// hub doesn't depend on the mjpeg one.
type h264espHubAdapter struct{ h *hub.Hub }

func (a *h264espHubAdapter) Subscribe() (h264esp.SourceSubscriber, error) {
	sub, err := a.h.Subscribe()
	if err != nil {
		return nil, err
	}
	return &h264espSubAdapter{s: sub}, nil
}

type h264espSubAdapter struct{ s *hub.Subscriber }

func (a *h264espSubAdapter) Frames() <-chan source.AccessUnit { return a.s.Frames() }
func (a *h264espSubAdapter) Close()                           { a.s.Close() }

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
