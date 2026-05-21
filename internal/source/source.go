// Package source defines the contract between the CARVILON streaming
// kernel and any upstream video source.
//
// The kernel — pion/WebRTC fan-out, MJPEG snapshot, signaling HTTP — knows
// nothing about how a source acquires video. UniFi Protect (today),
// generic RTSP (tomorrow), an ESP32 board (later) all plug in through the
// same VideoSource interface. Anything UniFi-specific lives in
// `internal/source/unifi`; anything RTSP-specific in a future
// `internal/source/rtsp`; etc.
//
// The interface is deliberately narrow: bring up a connection, deliver
// access units on a channel, expose the codec parameters needed to set up
// the WebRTC SDP, shut down cleanly. Nothing more.
package source

import (
	"context"
)

// AccessUnit is one complete H.264 frame, expressed as a slice of NAL
// units in decode order. NALs are raw bytes including the leading NAL
// header byte but without Annex-B start codes — the kernel adds those when
// marshaling for pion.
//
// PTS is the presentation timestamp in 90 kHz units, as produced by
// gortsplib's RTP-time decoder. Monotonic; safe to subtract for frame
// duration computation. Sources that come from a different transport
// supply an equivalent 90 kHz tick value.
type AccessUnit struct {
	NALUs [][]byte
	PTS   int64

	// IsKeyframe is true when this access unit can be decoded standalone
	// (contains an IDR slice). The kernel may treat keyframes specially —
	// for example, prepending SPS/PPS to outbound samples — but in general
	// the source is expected to deliver decoder-ready AUs.
	IsKeyframe bool
}

// H264Params carries the codec parameters that the kernel needs to fill
// in WebRTC SDP. SPS and PPS are raw NAL bytes (no Annex-B prefix).
// ProfileLevelID is the three-byte profile/level identifier as lowercase
// hex (e.g. "42e01f" for Constrained Baseline 3.1). Any field may be
// empty if the source has not yet negotiated or does not know it.
type H264Params struct {
	SPS            []byte
	PPS            []byte
	ProfileLevelID string
}

// VideoSource is a live H.264 video producer.
//
// Lifecycle:
//
//  1. NewSource(opts) — constructs but does not connect.
//  2. Start(ctx) — opens the underlying transport, negotiates, blocks until
//     access units are flowing. Returns the first error if setup failed.
//     After a successful Start, Frames() and Params() are valid.
//  3. The source produces AccessUnits on Frames() in a background goroutine.
//     If the underlying transport dies, Frames() closes — the kernel sees
//     end-of-stream by observing the closed channel.
//  4. Close() tears the source down. Subsequent reads of Frames() will see
//     it closed. Close is idempotent.
//
// Implementations MUST close the Frames channel exactly once, in any exit
// path (Close, ctx cancellation, fatal upstream error). The kernel uses
// channel-closed as its end-of-stream signal.
//
// Implementations SHOULD use a small buffered channel (e.g. cap 4) and a
// non-blocking send: dropping a frame when the consumer falls behind is
// the CARVILON drop-statt-buffer doctrine. The kernel reads as fast as
// pion accepts samples; back-pressure from the network must not back up
// into the source.
type VideoSource interface {
	Start(ctx context.Context) error
	Frames() <-chan AccessUnit
	Params() H264Params
	Close() error
}
