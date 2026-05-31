package whip

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/pion/webrtc/v4"

	"carvilon.local/stream/internal/icedebug"
	"carvilon.local/stream/internal/streamhub"
)

// trackReadyTimeout bounds how long a WHEP subscriber waits for the
// publisher's first RTP track to arrive before giving up with
// [ErrTrackNotReady]. A package var (not const) so tests can shrink it.
var trackReadyTimeout = 5 * time.Second

// Sentinel errors mapped to HTTP status by the WHEP handler.
var (
	// ErrNoPublisher means no active publisher exists for the streamID.
	// Handler -> 404.
	ErrNoPublisher = errors.New("whip: no active publisher for stream")
	// ErrTrackNotReady means a publisher session exists but its track
	// did not become ready within trackReadyTimeout. Handler -> 503.
	ErrTrackNotReady = errors.New("whip: publisher track not ready")
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

	me, err := newH264MediaEngine()
	if err != nil {
		return "", "", fmt.Errorf("whip: media engine: %w", err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me))

	// Cloud side runs on a public IP — host candidates suffice (no
	// STUN/TURN), same posture as the ingress.
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: nil})
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
