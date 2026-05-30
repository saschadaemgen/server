package whip

import "github.com/pion/webrtc/v4"

// newH264MediaEngine registers an H.264-only video MediaEngine. The two
// registered profiles (42001f and 42e01f, packetization-mode=1) mirror
// pion's own defaults, so a standard pion peer negotiates a codec
// without us pulling in VP8/VP9/Opus we don't need yet.
//
// Shared by WHIP ingress ([AcceptPublisher]) and WHEP egress
// ([AcceptSubscriber]) so the publisher and subscriber sides negotiate
// the SAME codec set — the publisher's fan-out track attaches to a
// subscriber PeerConnection only if both engines registered its codec.
// Keeping this in one place is what makes the symmetric negotiation
// reliable.
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
