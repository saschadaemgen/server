package stream

import (
	"fmt"
	"sync"

	"github.com/pion/webrtc/v4"

	"carvilon.local/stream/internal/droplog"
	"carvilon.local/stream/internal/profile"
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
	// Same gate as handleOffer: only h264_passthrough is a WebRTC
	// passthrough stream; the transcoded codecs have dedicated paths.
	if p.Codec != profile.CodecH264Passthrough {
		return nil, nil, fmt.Errorf("profile %q has codec=%q, not %q (TrackForStream is WebRTC passthrough only; use the MJPEG/H264-CBP paths for those)",
			p.Name, p.Codec, profile.CodecH264Passthrough)
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
	// feedTrack's stats client is nil here: this is an in-process bridge,
	// not an HTTP viewer, so it does not appear in /stream/stats. The
	// counters are nil-safe (S6-15). The same idle-timeout watchdog
	// applies, so a wedged upstream still tears the feed down.
	go s.feedTrack(sub, track, drops, nil)

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
