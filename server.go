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
)

//go:embed web
var webFS embed.FS

// Server hosts the WebRTC signaling HTTP endpoint plus a tiny static test
// page that exercises it from a browser. One [Source] feeds any number of
// peer connections through the source's shared track.
//
// The signaling protocol is intentionally minimal: POST the browser's offer
// SDP to /offer with Content-Type application/sdp, receive the answer SDP
// in the response body. No trickle ICE, no auth — spike scope only.
type Server struct {
	src    *Source
	addr   string
	logger *log.Logger

	api *webrtc.API
	srv *http.Server
}

// ServerOptions configures a [Server].
type ServerOptions struct {
	// Source is the video source whose track this server hands out to peers.
	// Must already have been Started.
	Source *Source

	// Addr is the HTTP listen address, e.g. ":8555". Avoid 9080
	// (carvilon-server) and 1984 (go2rtc).
	Addr string

	// Logger receives diagnostic output. If nil, the default logger is used.
	Logger *log.Logger
}

// NewServer builds a Server. It does not listen — call [Server.ListenAndServe].
func NewServer(opts ServerOptions) (*Server, error) {
	if opts.Source == nil {
		return nil, errors.New("stream: Source is required")
	}
	if opts.Addr == "" {
		return nil, errors.New("stream: Addr is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}

	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		return nil, fmt.Errorf("stream: register codecs: %w", err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me))

	s := &Server{
		src:    opts.Source,
		addr:   opts.Addr,
		logger: logger,
		api:    api,
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

// ListenAndServe starts the HTTP server and blocks until it stops. The
// returned error is never nil — it is at least [http.ErrServerClosed] on a
// clean shutdown.
func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("stream: listen %s: %w", s.addr, err)
	}
	s.logger.Printf("stream: signaling + test page on http://%s", ln.Addr())
	return s.srv.Serve(ln)
}

// Shutdown stops the HTTP server gracefully.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

// handleOffer implements the POST /offer endpoint: read the browser's offer
// SDP, attach the source's track to a fresh PeerConnection, return the
// answer SDP with all ICE candidates already gathered.
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

	track := s.src.Track()
	if track == nil {
		http.Error(w, "source not ready", http.StatusServiceUnavailable)
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

	if _, err := pc.AddTrack(track); err != nil {
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
