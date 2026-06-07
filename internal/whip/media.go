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

// RTX (RFC 4588) retransmission payload types for the WHEP egress, paired
// with the two H.264 codecs (apt=102 -> 103, apt=106 -> 107). The values
// mirror pion's RegisterDefaultCodecs / Chromium convention; pion matches rtx
// to its apt by MimeType, and the answer reuses the PT mapping from the offer.
const (
	rtxPayloadTypeFor102 webrtc.PayloadType = 103
	rtxPayloadTypeFor106 webrtc.PayloadType = 107
)

// newH264MediaEngineWithRTX is the EGRESS MediaEngine: [newH264MediaEngine]
// plus the rtx codecs paired with each H.264 payload type.
//
// Registering rtx is the ONE server-side step that lets the WHEP egress ACCEPT
// the rtx that Android/Chromium already offers by default. Without it pion
// drops the offered rtx from the answer (the rtx codec is unknown to the
// engine), so retransmissions fall back to the SAME SSRC - which the
// receiver's SRTP replay guard discards, which is why the climbing NACK count
// produced no recovery (S4 RTX befund).
//
// Once rtx is negotiated, pion v4 wires the rest AUTOMATICALLY: the RTPSender
// allocates an RTX SSRC (isRTXEnabled), the answer advertises
// a=ssrc-group:FID <media> <rtx>, the [webrtc.TrackLocalStaticRTP] binding
// picks up SSRCRetransmission + the rtx PT, and the existing NACK responder's
// retransmissions go out on the RTX stream (separate SSRC, replay-safe). No
// explicit RTX-sender setup is required here.
//
// Order matters (pion requirement): H.264 (102, 106) is registered first by
// [newH264MediaEngine], THEN the paired rtx, so each rtx matches its apt.
// FlexFEC is layered on later in [AcceptSubscriber] via ConfigureFlexFEC03 -
// the hybrid is intentional (FEC proactively against residual loss, RTX
// reactively against the bursts).
func newH264MediaEngineWithRTX() (*webrtc.MediaEngine, error) {
	me, err := newH264MediaEngine()
	if err != nil {
		return nil, err
	}
	rtxCodecs := []webrtc.RTPCodecParameters{
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeRTX, ClockRate: 90000, SDPFmtpLine: "apt=102"},
			PayloadType:        rtxPayloadTypeFor102,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeRTX, ClockRate: 90000, SDPFmtpLine: "apt=106"},
			PayloadType:        rtxPayloadTypeFor106,
		},
	}
	for _, c := range rtxCodecs {
		if err := me.RegisterCodec(c, webrtc.RTPCodecTypeVideo); err != nil {
			return nil, err
		}
	}
	return me, nil
}
