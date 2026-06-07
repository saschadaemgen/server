package whip

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/flexfec"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/webrtc/v4"

	"carvilon.local/stream/internal/icedebug"
	"carvilon.local/stream/internal/streamhub"
)

// trackReadyTimeout bounds how long a WHEP subscriber waits for the
// publisher's first RTP track to arrive before giving up with
// [ErrTrackNotReady]. A package var (not const) so tests can shrink it.
var trackReadyTimeout = 5 * time.Second

// egressNackBufferSize is the NACK responder send-buffer in packets on the
// WHEP egress (S4 resilience step 1). 4096 (a power of two, required by the
// responder's ring buffer) is ~6.3s at the measured ~648 pkts/s - more than
// one full GOP (~5s) - so a late retransmit is never a cache miss. pion's
// default is 1024 (~1.5s); this sets it explicitly and generously to rule
// out the cache-depth hypothesis.
const egressNackBufferSize = 4096

// FlexFEC-03 forward error correction on the WHEP egress (S4 resilience
// step 2): recovers losses WITHOUT retransmission, for the far/mobile case
// where NACK round-trips are too slow. It is purely additive (repair packets
// on a separate SSRC); the camera video bitrate is untouched (passthrough, no
// encoder), so there is no quality/resolution reduction. Peers that do not
// accept flexfec-03 (e.g. whep-probe) simply omit it from the answer and get
// no FEC - it never breaks them.
const (
	// flexFECPayloadType is the dynamic RTP payload type for the flexfec-03
	// repair stream (102/106 are the H264 codecs in newH264MediaEngine; 120
	// is free).
	flexFECPayloadType webrtc.PayloadType = 120
	// flexFECMediaPackets / flexFECRepairPackets: ~50% FEC overhead (5 repair
	// per 10 media). RAISED from the initial 10:2 (~20%) after the cloud/4G
	// field test: with 10:2 FlexFEC flowed and 960x1280 arrived, but residual
	// loss stayed (lost 0->50, freeze 0->13) - 20% did not cover the periodic
	// short-GOP keyframe bursts. pion FlexFEC is 1D, so burst coverage scales
	// with NumFECPackets PER GROUP; this is an iterative knob. Next step if
	// still short: a SMALLER group at high rate (e.g. 8:4 / 6:4) covers longer
	// bursts relative to the group size.
	flexFECMediaPackets  = 10
	flexFECRepairPackets = 5
)

// Sentinel errors mapped to HTTP status by the WHEP handler.
var (
	// ErrNoPublisher means no active publisher exists for the streamID.
	// Handler -> 404.
	ErrNoPublisher = errors.New("whip: no active publisher for stream")
	// ErrTrackNotReady means a publisher session exists but its track
	// did not become ready within trackReadyTimeout. Handler -> 503.
	ErrTrackNotReady = errors.New("whip: publisher track not ready")
)

// coldPublishTimeout bounds the wait for a publisher to dock after a cold
// WHEP subscribe triggered a request_publish (the round trip request_publish
// -> side-channel -> edge whipclient -> POST /whip -> hub session). A package
// var so tests can shrink it. The per-track readiness wait (trackReadyTimeout)
// applies on top, inside AcceptSubscriber.
var coldPublishTimeout = 12 * time.Second

// Cold-start sentinels (WHEP trigger path), both mapped to 504 by handleWHEP
// (not 404: the trigger DID run, so this is an upstream failure, not a
// missing route).
var (
	// errNoEdge means the RequestPublish callback reached no edge (edges < 1),
	// so the stream cannot start; fail fast without waiting.
	errNoEdge = errors.New("whip: request_publish reached no edge")
	// errColdPublishTimeout means an edge accepted the request_publish but no
	// publisher docked within coldPublishTimeout.
	errColdPublishTimeout = errors.New("whip: publisher did not dock after request_publish")
)

// AcceptSubscriber takes a WHEP SDP offer and builds a PeerConnection
// that sends the hub's fan-out track to the subscriber. It returns the
// SDP answer and the generated session id (used in the WHEP Location
// header).
//
// It waits (up to [trackReadyTimeout]) for the publisher's track to
// become ready before attaching it, resolving the race where a
// subscriber arrives between the publisher's hub-Add and its first
// OnTrack callback.
//
// Subscriber teardown (ICE failure) closes ONLY this subscriber
// PeerConnection — it never touches the publisher's hub entry, so the
// publisher and all other subscribers keep running.
func AcceptSubscriber(
	ctx context.Context,
	hub *streamhub.Hub,
	logger *log.Logger,
	streamID string,
	sdpOffer string,
	iceServers []webrtc.ICEServer,
) (sdpAnswer string, sessionID string, err error) {
	sess, ok := hub.Get(streamID)
	if !ok {
		return "", "", ErrNoPublisher
	}

	// Wait for the publisher's first RTP track. This is the race fix:
	// without it, a subscriber arriving early would attach a nil track.
	waitCtx, cancel := context.WithTimeout(ctx, trackReadyTimeout)
	defer cancel()
	track, err := sess.WaitTrack(waitCtx)
	if err != nil {
		return "", "", ErrTrackNotReady
	}

	// TODO egress-auth: the egress-token check lands here, BEFORE the
	// track is attached. Master decision (Option 3): S2-05 ships the
	// hook pass-through — no subscriber-identity scheme is settled yet,
	// so we deliberately do NOT gate here. Real spec arrives with the
	// SDP/ICE season.

	// EGRESS engine: H.264 + rtx (RFC 4588). Registering rtx is what lets the
	// answer negotiate the rtx the phone already offers, so the NACK responder
	// below retransmits over a SEPARATE RTX SSRC (replay-safe) instead of the
	// same SSRC (which the receiver's replay guard drops). See
	// newH264MediaEngineWithRTX for the full wiring befund.
	me, err := newH264MediaEngineWithRTX()
	if err != nil {
		return "", "", fmt.Errorf("whip: media engine: %w", err)
	}
	// S4 loss recovery: register the NACK responder on the EGRESS
	// PeerConnection so the subscriber's NACKs are answered with retransmits.
	// Without an interceptor registry pion registers NONE, so the NACKs the
	// phone sends (seen climbing in getStats) were ignored -> progressive
	// freezing on a lossy radio leg.
	//
	// S4 step 1: construct the responder EXPLICITLY with a generous send
	// buffer (egressNackBufferSize) instead of webrtc.ConfigureNack's default
	// (1024 ~= 1.5s), to rule out definitively that a retransmit arrives too
	// late / has fallen out of the cache. The generator is omitted: the egress
	// is sendonly, so it has no inbound media to NACK. The nack + nack pli
	// rtcp-fb stays on the H264 codec (newH264MediaEngine), so the answer
	// still advertises it (asserted in TestWHEP_Happy). Edge->cloud is
	// loss-free (measured), so every packet reaches the egress send-cache and
	// can be retransmitted on the same SSRC.
	ir := &interceptor.Registry{}
	// S4 step 2: FlexFEC-03 FEC. Registered FIRST - pion requires the FEC
	// interceptor before any RTP-modifying interceptor (so its repair packets
	// are not altered). The egress sequence numbers are clean and monotone
	// (the edge publishes via TrackLocalStaticSample, the cloud forwards them
	// verbatim), which is the well-formed input FlexFEC needs. Coexists with
	// the NACK responder below; a peer that does not offer flexfec-03 just
	// gets no FEC.
	if err := webrtc.ConfigureFlexFEC03(flexFECPayloadType, me, ir,
		flexfec.NumMediaPackets(flexFECMediaPackets),
		flexfec.NumFECPackets(flexFECRepairPackets),
	); err != nil {
		return "", "", fmt.Errorf("whip: configure flexfec-03: %w", err)
	}
	responder, err := nack.NewResponderInterceptor(nack.ResponderSize(egressNackBufferSize))
	if err != nil {
		return "", "", fmt.Errorf("whip: nack responder: %w", err)
	}
	ir.Add(responder)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithInterceptorRegistry(ir))

	// S3 TURN: iceServers is the relay list the server minted (TURN URLs +
	// fresh ephemeral creds) when TURN is on; nil -> host candidates only.
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: iceServers})
	if err != nil {
		return "", "", fmt.Errorf("whip: new peer connection: %w", err)
	}

	sessionID, err = randomSessionID()
	if err != nil {
		_ = pc.Close()
		return "", "", fmt.Errorf("whip: session id: %w", err)
	}

	// Attach the publisher's fan-out track. One TrackLocalStaticRTP can
	// bind to many PeerConnections; each gets its own RTP writer. This
	// is the fan-out.
	rtpSender, err := pc.AddTrack(track)
	if err != nil {
		_ = pc.Close()
		return "", "", fmt.Errorf("whip: add track: %w", err)
	}
	// Drain the sender's inbound RTCP (receiver reports, NACK, PLI) so
	// the interceptor chain can process it; without this read the sender
	// buffer would back up. Exits when the PC closes.
	go drainRTCP(rtpSender)

	// S3 ICE befund: opt-in masked candidate + state logging
	// (CARVILON_ICE_DEBUG). Purely additive; no-op when the flag is off.
	icedebug.AttachCandidateLogging(pc, logger, "whep sid="+streamID)
	iceTracker := icedebug.NewStateTracker(logger, "whep sid="+streamID)

	// Subscriber-only teardown: close THIS PeerConnection, never the
	// hub's publisher entry. The publisher and other subscribers are
	// unaffected.
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		logger.Printf("whep: sid=%s session=%s ICE state=%s", streamID, sessionID, state)
		iceTracker.Log(state)
		switch state {
		case webrtc.ICEConnectionStateFailed,
			webrtc.ICEConnectionStateDisconnected,
			webrtc.ICEConnectionStateClosed:
			_ = pc.Close()
		}
	})

	// SDP negotiation (mirror of the ingress, sending instead of
	// receiving).
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdpOffer,
	}); err != nil {
		_ = pc.Close()
		return "", "", fmt.Errorf("whip: set remote description: %w", err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		_ = pc.Close()
		return "", "", fmt.Errorf("whip: create answer: %w", err)
	}
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		_ = pc.Close()
		return "", "", fmt.Errorf("whip: set local description: %w", err)
	}
	select {
	case <-gatherComplete:
	case <-ctx.Done():
		_ = pc.Close()
		return "", "", fmt.Errorf("whip: ICE gathering: %w", ctx.Err())
	case <-time.After(gatherTimeout):
		_ = pc.Close()
		return "", "", errors.New("whip: ICE gathering timed out")
	}

	logger.Printf("whep: sid=%s session=%s subscriber attached", streamID, sessionID)
	return pc.LocalDescription().SDP, sessionID, nil
}

// waitForPublisherSession blocks until a publisher session is registered in
// the hub for streamID, or ctx is done. The hub exposes no session-arrival
// signal, so it polls Get (a cheap RLock map read) on a short ticker. It
// returns true once a session exists, false on ctx timeout/cancel. The
// per-track readiness wait stays in AcceptSubscriber (WaitTrack).
func waitForPublisherSession(ctx context.Context, hub *streamhub.Hub, streamID string) bool {
	if _, ok := hub.Get(streamID); ok {
		return true
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if _, ok := hub.Get(streamID); ok {
				return true
			}
		}
	}
}

// drainRTCP reads and discards inbound RTCP on an egress RTP sender
// until the sender (its PeerConnection) closes. Required so pion's
// interceptor chain advances; the packets themselves are not needed
// here yet (PLI handling on the ingress is future work).
func drainRTCP(sender *webrtc.RTPSender) {
	buf := make([]byte, 1500)
	for {
		if _, _, err := sender.Read(buf); err != nil {
			return
		}
	}
}
