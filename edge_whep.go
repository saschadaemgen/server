package stream

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/flexfec"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/webrtc/v4"

	"carvilon.local/stream/internal/droplog"
	"carvilon.local/stream/internal/profile"
	"carvilon.local/stream/internal/publishtoken"
	"carvilon.local/stream/internal/stats"
)

// LAN-direct WHEP resilience knobs. They mirror the cloud egress values in
// internal/whip (NACK responder + FlexFEC-03) so the LAN path has the same
// loss-recovery parity; on a clean one-WLAN-hop LAN they rarely fire, but
// they cost nothing and keep the two egress paths symmetric.
const (
	edgeWHEPFlexFECPayloadType   webrtc.PayloadType = 120
	edgeWHEPFlexFECMediaPackets                     = 10
	edgeWHEPFlexFECRepairPackets                    = 2
	edgeWHEPNackBufferSize                          = 4096
)

// newLANWHEPAPI builds the webrtc.API for the edge LAN-direct WHEP endpoint.
// Its SettingEngine is pinned to a FIXED UDP port (icePort) and advertises
// ONLY the real private-IPv4 LAN host candidates - no SetNAT1To1IPs, so the
// edge offers its genuine 192.168 host (ICE type-pref 126), which a same-WLAN
// phone picks over the VPS srflx (100). NACK + FlexFEC ride the same registry.
//
// The UDP mux binds icePort ONCE (server lifetime) and is shared across all
// LAN WHEP subscribers. Filtering by IP (private IPv4) rather than interface
// name avoids guessing the RPi's interface names (eth0/wlan0/end0/...).
func newLANWHEPAPI(icePort int) (*webrtc.API, ice.UDPMux, error) {
	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		return nil, nil, fmt.Errorf("stream: lan-whep codecs: %w", err)
	}

	ir := &interceptor.Registry{}
	// FlexFEC first - pion requires the FEC interceptor before any
	// RTP-modifying interceptor.
	if err := webrtc.ConfigureFlexFEC03(edgeWHEPFlexFECPayloadType, me, ir,
		flexfec.NumMediaPackets(edgeWHEPFlexFECMediaPackets),
		flexfec.NumFECPackets(edgeWHEPFlexFECRepairPackets),
	); err != nil {
		return nil, nil, fmt.Errorf("stream: lan-whep flexfec: %w", err)
	}
	responder, err := nack.NewResponderInterceptor(nack.ResponderSize(edgeWHEPNackBufferSize))
	if err != nil {
		return nil, nil, fmt.Errorf("stream: lan-whep nack: %w", err)
	}
	ir.Add(responder)

	se := webrtc.SettingEngine{}
	mux, err := ice.NewMultiUDPMuxFromPort(icePort)
	if err != nil {
		return nil, nil, fmt.Errorf("stream: lan-whep udp mux on :%d: %w", icePort, err)
	}
	se.SetICEUDPMux(mux)
	// Advertise ONLY the real private-IPv4 LAN host candidates: no IPv6 noise,
	// no NAT1To1 rewrite. The edge announces its true 192.168 host.
	se.SetIPFilter(func(ip net.IP) bool { return ip.To4() != nil && ip.IsPrivate() })

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(me),
		webrtc.WithInterceptorRegistry(ir),
		webrtc.WithSettingEngine(se),
	)
	return api, mux, nil
}

// handleEdgeWHEP implements POST /whep/{streamID} on the edge: a WHEP
// subscriber (a phone on the same WLAN) POSTs an SDP offer and receives the
// camera's H.264 passthrough track, answered over the LAN-host SettingEngine
// so ICE takes the direct LAN path instead of the VPS relay.
//
// streamID is resolved as a browser (h264_passthrough) profile name - the
// same resolution /offer does for ?src=. Egress auth: when an egress key is
// configured it verifies a Bearer egress_token (fail-closed), mirroring the
// cloud egress; with no key it is open, like /offer on the LAN today. The
// route is only registered when the LAN WHEP endpoint is enabled, so s.whepAPI
// is never nil here.
func (s *Server) handleEdgeWHEP(w http.ResponseWriter, r *http.Request) {
	streamID := r.PathValue("streamID")
	if streamID == "" {
		http.Error(w, "missing streamID", http.StatusBadRequest)
		return
	}

	// Egress auth: verify-if-present, fail-closed when a key is set.
	if len(s.egressKey) > 0 {
		const bearerPrefix = "Bearer "
		authz := r.Header.Get("Authorization")
		if !strings.HasPrefix(authz, bearerPrefix) {
			s.logger.Printf("edge whep: sid=%s rejected: missing bearer", streamID)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err := publishtoken.Verify(strings.TrimPrefix(authz, bearerPrefix), streamID, s.egressKey, time.Now().UTC()); err != nil {
			s.logger.Printf("edge whep: sid=%s rejected: %v", streamID, err)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	p, err := s.profiles.Get(streamID)
	if err != nil {
		if errors.Is(err, profile.ErrUnknownProfile) {
			http.Error(w, "unknown profile", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if p.Codec != profile.CodecH264Passthrough {
		http.Error(w, fmt.Sprintf("profile %q has codec=%q, not %q", p.Name, p.Codec, profile.CodecH264Passthrough), http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(body) == 0 {
		http.Error(w, "read offer", http.StatusBadRequest)
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

	pc, err := s.whepAPI.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		sub.Close()
		http.Error(w, "create peer: "+err.Error(), http.StatusInternalServerError)
		return
	}
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		s.logger.Printf("edge whep: viewer %s/%d state=%s", p.Name, sub.ID(), st)
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
		Label:  fmt.Sprintf("edge whep: viewer %s/%d writesample", p.Name, sub.ID()),
	}
	go func() {
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
	w.Header().Set("Location", fmt.Sprintf("/whep/%s/session/%d", streamID, sub.ID()))
	w.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(w, pc.LocalDescription().SDP)
}
