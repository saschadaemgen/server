package whip

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/pion/webrtc/v4"

	"carvilon.local/stream/internal/streamhub"
)

// gatherTimeout bounds the wait for ICE candidate gathering before the
// SDP answer is returned. With host-only candidates on a public-IP VPS
// gathering is near-instant; the timeout only guards a stuck stack.
const gatherTimeout = 5 * time.Second

// AcceptPublisher takes a WHIP SDP offer and builds a PeerConnection
// that receives RTP from the publisher and forwards it into a
// [webrtc.TrackLocalStaticRTP] stored in the hub. It returns the SDP
// answer to send back and the generated session id (used in the WHIP
// Location header).
//
// The logger is threaded in (the briefing's contract omitted it, but
// the OnTrack/pump and ICE-state callbacks must log async failures —
// there is no other channel to surface them). On any setup error the
// PeerConnection is closed before returning. A duplicate publish for an
// already-active streamID returns [streamhub.ErrConflict] unchanged so
// the HTTP layer can map it to 409.
func AcceptPublisher(
	ctx context.Context,
	hub *streamhub.Hub,
	logger *log.Logger,
	streamID string,
	sdpOffer string,
) (sdpAnswer string, sessionID string, err error) {
	me, err := newH264MediaEngine()
	if err != nil {
		return "", "", fmt.Errorf("whip: media engine: %w", err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me))

	// Cloud side runs on a public IP — host candidates suffice, so no
	// STUN/TURN. Client-side ICE is a later season (port planning open).
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: nil})
	if err != nil {
		return "", "", fmt.Errorf("whip: new peer connection: %w", err)
	}

	sessionID, err = randomSessionID()
	if err != nil {
		_ = pc.Close()
		return "", "", fmt.Errorf("whip: session id: %w", err)
	}

	sess := &streamhub.Session{
		StreamID: streamID,
		PC:       pc,
		OnClose:  func() { _ = pc.Close() },
	}

	// OnTrack: wrap the incoming remote RTP in a local fan-out track,
	// store it on the session, and pump packets across until the remote
	// ends. The session pointer is captured directly (it is the same
	// object the hub stores after Add below).
	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		local, terr := webrtc.NewTrackLocalStaticRTP(remote.Codec().RTPCodecCapability, "video", streamID)
		if terr != nil {
			logger.Printf("whip: sid=%s new local track: %v", streamID, terr)
			return
		}
		sess.Track = local
		logger.Printf("whip: sid=%s track started codec=%s", streamID, remote.Codec().MimeType)
		go pumpRTP(remote, local, logger, streamID)
	})

	// Auto-cleanup on connection death — the hub removes itself, no
	// session leak. OnClose (pc.Close) runs exactly once via Remove.
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		logger.Printf("whip: sid=%s ICE state=%s", streamID, state)
		switch state {
		case webrtc.ICEConnectionStateFailed,
			webrtc.ICEConnectionStateDisconnected,
			webrtc.ICEConnectionStateClosed:
			hub.Remove(streamID)
		}
	})

	// SDP negotiation.
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
	// Capture the gathering promise BEFORE SetLocalDescription (which
	// starts gathering), then wait so the returned answer carries all
	// host candidates (non-trickle WHIP).
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

	// Register only after a clean answer, so a conflicting late publish
	// can never tear down the already-healthy publisher.
	if err := hub.Add(sess); err != nil {
		_ = pc.Close()
		return "", "", err // ErrConflict bubbles up to a 409
	}

	return pc.LocalDescription().SDP, sessionID, nil
}

// pumpRTP forwards RTP packets from the publisher's remote track into
// the local fan-out track until either side ends. Read/write errors end
// the loop quietly (they're the normal teardown signal); anything
// surprising is logged.
func pumpRTP(remote *webrtc.TrackRemote, local *webrtc.TrackLocalStaticRTP, logger *log.Logger, streamID string) {
	for {
		pkt, _, err := remote.ReadRTP()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				logger.Printf("whip: sid=%s rtp read ended: %v", streamID, err)
			}
			return
		}
		if err := local.WriteRTP(pkt); err != nil {
			if !errors.Is(err, io.ErrClosedPipe) {
				logger.Printf("whip: sid=%s rtp write ended: %v", streamID, err)
			}
			return
		}
	}
}

// newH264MediaEngine registers an H.264-only video MediaEngine. The two
// registered profiles (42001f and 42e01f, packetization-mode=1) mirror
// pion's own defaults, so a standard pion publisher negotiates a codec
// without us pulling in VP8/VP9/Opus we don't need yet.
func newH264MediaEngine() (*webrtc.MediaEngine, error) {
	me := &webrtc.MediaEngine{}
	videoRTCPFeedback := []webrtc.RTCPFeedback{
		{Type: "goog-remb"},
		{Type: "ccm", Parameter: "fir"},
		{Type: "nack"},
		{Type: "nack", Parameter: "pli"},
	}
	codecs := []webrtc.RTPCodecParameters{
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     webrtc.MimeTypeH264,
				ClockRate:    90000,
				SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f",
				RTCPFeedback: videoRTCPFeedback,
			},
			PayloadType: 102,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     webrtc.MimeTypeH264,
				ClockRate:    90000,
				SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
				RTCPFeedback: videoRTCPFeedback,
			},
			PayloadType: 106,
		},
	}
	for _, c := range codecs {
		if err := me.RegisterCodec(c, webrtc.RTPCodecTypeVideo); err != nil {
			return nil, err
		}
	}
	return me, nil
}

// randomSessionID returns 16 hex characters (8 random bytes) for the
// WHIP Location header path component.
func randomSessionID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
