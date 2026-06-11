package stream

import (
	"fmt"
	"sync"

	"github.com/pion/webrtc/v4"

	"carvilon.local/stream/internal/droplog"
	"carvilon.local/stream/internal/profile"
	"carvilon.local/stream/internal/stats"
)

// TrackForStream opens a fan-out subscriber for the named profile and
// returns a playable WebRTC track plus a stop function. The track is fed
// by the same access-unit -> sample mechanism as the local browser path
// ([Server.feedTrack]). It is intended for in-process consumers like the
// carvilon-edge WHIP publisher (the cloud-push path).
//
// profileName is the resolved profile string (e.g. intercom_web,
// intercom_android). Unknown -> [profile.ErrUnknownProfile]. Wrong codec
// (not h264_passthrough) -> error: the cloud push path is WebRTC
// passthrough only; mjpeg / h264_cbp have their own local paths.
//
// The returned track is a [webrtc.TrackLocalStaticSample] under the
// hood, returned as the [webrtc.TrackLocal] interface so callers attach
// it directly via pc.AddTrack. The stop function unsubscribes from the
// hub (releasing the shared upstream pull when this was the last
// consumer); it is idempotent.
//
// Because the subscribe goes through the shared s.sources registry, this
// consumer shares the SAME camera pull as every other consumer of the
// same (CameraID, Quality, Encryption) key — no second RTSPS pull. That
// sharing is the whole point of the in-process embedding.
func (s *Server) TrackForStream(profileName string) (webrtc.TrackLocal, func(), error) {
	p, err := s.profiles.Get(profileName)
	if err != nil {
		return nil, nil, err // ErrUnknownProfile bubbles up unchanged
	}
	// The cloud-push path ships H.264 over WebRTC. Two codecs qualify:
	// the raw camera passthrough, and the S4 short-GOP re-encode (still
	// H.264, just re-encoded with a small GOP for the 4G link). The
	// transcoded ESP/MJPEG codecs have their own local paths. NOTE: the
	// LAN endpoints (handleOffer, handleEdgeWHEP) stay passthrough-only on
	// purpose — re-encoding for a LAN viewer would burn the edge encoder
	// for no benefit; the re-encode exists solely for the cloud/4G egress.
	if p.Codec != profile.CodecH264Passthrough && p.Codec != profile.CodecH264ReencodeShortGOP {
		return nil, nil, fmt.Errorf("profile %q has codec=%q (TrackForStream serves %q or %q over WebRTC; use the MJPEG/H264-CBP paths for those)",
			p.Name, p.Codec, profile.CodecH264Passthrough, profile.CodecH264ReencodeShortGOP)
	}

	srcHub := s.sources.HubFor(s.sourceKeyFor(p))
	sub, err := srcHub.Subscribe()
	if err != nil {
		return nil, nil, fmt.Errorf("subscribe: %w", err)
	}

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: h264ClockRate},
		fmt.Sprintf("video-%s-%d", p.Name, sub.ID()),
		fmt.Sprintf("carvilon-%s-%d", p.Name, sub.ID()),
	)
	if err != nil {
		sub.Close()
		return nil, nil, fmt.Errorf("create track: %w", err)
	}

	drops := &droplog.Counter{
		Logger: s.logger,
		Label:  fmt.Sprintf("stream: trackforstream %s/%d writesample", p.Name, sub.ID()),
	}
	// S20: the publish bridge IS edge egress, so it registers in
	// /stream/stats — as an UPLINK, never as a viewer (RegisterUplink
	// keeps it out of every consumer count; the admin shows cloud
	// viewers via the side-channel WHEP counts instead). The profile row
	// thus lives while a publish session stands: output fps, bitrate and
	// drops come from this sender. Unregister mirrors handleOffer: it
	// runs when feedTrack returns — on stop() closing the subscription
	// AND on the idle watchdog — so a dead publish can never linger as a
	// ghost-active row.
	var sc *stats.Client
	if s.stats != nil {
		sc = s.stats.RegisterUplink(p.Name, string(p.Codec), "whip-uplink")
	}
	go func() {
		if s.stats != nil {
			defer s.stats.Unregister(sc)
		}
		s.feedTrack(sub, track, drops, sc)
	}()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			// Closing the subscriber shuts the hub frames channel, which
			// ends the feedTrack loop; if this was the last subscriber on
			// the camera key, the shared pull stops.
			sub.Close()
		})
	}

	return track, stop, nil
}
