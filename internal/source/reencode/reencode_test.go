package reencode

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"carvilon.local/stream/internal/source"
)

func hasArg(args []string, s string) bool {
	for _, a := range args {
		if a == s {
			return true
		}
	}
	return false
}

func hasPair(args []string, a, b string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == a && args[i+1] == b {
			return true
		}
	}
	return false
}

func hasSubstr(args []string, sub string) bool {
	for _, a := range args {
		if strings.Contains(a, sub) {
			return true
		}
	}
	return false
}

// TestBuildFFmpegArgs_ApprovedDecisions pins the Stufe-C decisions into the
// argv so a later careless edit cannot silently undo them. The hard negatives
// matter most: NO intra-refresh (WebRTC needs IDRs - intra-refresh = black
// screen), NO -force_key_frames, NO low_delay (D-0002 Canary), and NOT back to
// v4l2m2m (which has no VBV).
func TestBuildFFmpegArgs_ApprovedDecisions(t *testing.T) {
	args := buildFFmpegArgs(20, 1500, 600)

	if !hasPair(args, "-c:v", "libx264") {
		t.Errorf("missing -c:v libx264; got %v", args)
	}
	if !hasPair(args, "-preset", "ultrafast") {
		t.Errorf("missing -preset ultrafast; got %v", args)
	}
	if !hasPair(args, "-tune", "zerolatency") {
		t.Errorf("missing -tune zerolatency; got %v", args)
	}
	// S6-04: single-slice (else h264esp.AUSplitter mis-frames each slice).
	if !hasPair(args, "-x264-params", "sliced-threads=0:slices=1") {
		t.Errorf("missing -x264-params sliced-threads=0:slices=1; got %v", args)
	}
	if !hasPair(args, "-b:v", "1500k") {
		t.Errorf("missing -b:v 1500k; got %v", args)
	}
	// VBV window - the Stufe-C lever.
	if !hasPair(args, "-maxrate", "1500k") {
		t.Errorf("missing -maxrate 1500k; got %v", args)
	}
	if !hasPair(args, "-bufsize", "600k") {
		t.Errorf("missing -bufsize 600k; got %v", args)
	}
	if !hasPair(args, "-g", "20") {
		t.Errorf("missing -g 20 (periodic IDRs); got %v", args)
	}
	if !hasPair(args, "-bf", "0") {
		t.Errorf("missing -bf 0; got %v", args)
	}
	if !hasPair(args, "-threads", "4") {
		t.Errorf("missing -threads 4 (SW multi-core decode); got %v", args)
	}
	if !hasArg(args, "-an") {
		t.Errorf("missing -an (VIDEO-ONLY); got %v", args)
	}
	if !hasPair(args, "-i", "pipe:0") {
		t.Errorf("missing -i pipe:0; got %v", args)
	}
	if !hasSubstr(args, "dump_extra=freq=keyframe") {
		t.Errorf("missing dump_extra=freq=keyframe (in-band SPS/PPS); got %v", args)
	}

	// HARD NEGATIVES:
	if hasSubstr(args, "intra-refresh") {
		t.Errorf("intra-refresh present; WebRTC needs IDRs, intra-refresh = black screen: %v", args)
	}
	if hasArg(args, "-force_key_frames") {
		t.Errorf("-force_key_frames present; GOP must be -g driven: %v", args)
	}
	if hasArg(args, "h264_v4l2m2m") {
		t.Errorf("h264_v4l2m2m present; Stufe C uses libx264 for the VBV window: %v", args)
	}
	if hasSubstr(args, "low_delay") {
		t.Errorf("low_delay present; forbidden (D-0002 Canary): %v", args)
	}
	// No -profile:v: libx264 default profile is fine (matches camera +
	// passthrough, Android decodes); a forced profile is a fallback only.
	if hasArg(args, "-profile:v") {
		t.Errorf("-profile:v present; libx264 default is intended: %v", args)
	}
}

func TestBuildFFmpegArgs_VBVThreaded(t *testing.T) {
	a := buildFFmpegArgs(25, 2500, 400)
	if !hasPair(a, "-g", "25") || !hasPair(a, "-b:v", "2500k") ||
		!hasPair(a, "-maxrate", "2500k") || !hasPair(a, "-bufsize", "400k") {
		t.Errorf("gop/bitrate/VBV not threaded through: %v", a)
	}
}

func TestSplitAnnexB(t *testing.T) {
	sc4 := []byte{0, 0, 0, 1}
	sc3 := []byte{0, 0, 1}

	t.Run("keyframe SPS+PPS+IDR", func(t *testing.T) {
		var au []byte
		au = append(au, sc4...)
		au = append(au, 0x67, 0x11) // SPS  (type 7)
		au = append(au, sc4...)
		au = append(au, 0x68, 0x22) // PPS  (type 8)
		au = append(au, sc4...)
		au = append(au, 0x65, 0x33) // IDR  (type 5)
		nalus, key := splitAnnexB(au)
		if len(nalus) != 3 {
			t.Fatalf("got %d NALs, want 3 (%v)", len(nalus), nalus)
		}
		if !key {
			t.Error("AU containing an IDR must be a keyframe")
		}
		if nalus[0][0] != 0x67 || nalus[1][0] != 0x68 || nalus[2][0] != 0x65 {
			t.Errorf("NAL headers wrong: %x %x %x", nalus[0][0], nalus[1][0], nalus[2][0])
		}
	})

	t.Run("P-frame single non-IDR", func(t *testing.T) {
		au := append(append([]byte{}, sc4...), 0x41, 0xAB) // type 1
		nalus, key := splitAnnexB(au)
		if len(nalus) != 1 {
			t.Fatalf("got %d NALs, want 1", len(nalus))
		}
		if key {
			t.Error("non-IDR AU must not be a keyframe")
		}
	})

	t.Run("mixed 3- and 4-byte start codes", func(t *testing.T) {
		var au []byte
		au = append(au, sc3...)
		au = append(au, 0x67, 0xAA)
		au = append(au, sc4...)
		au = append(au, 0x65, 0xBB)
		nalus, key := splitAnnexB(au)
		if len(nalus) != 2 {
			t.Fatalf("got %d NALs, want 2", len(nalus))
		}
		if !key {
			t.Error("expected keyframe (IDR present)")
		}
		if len(nalus[1]) != 2 || nalus[1][0] != 0x65 || nalus[1][1] != 0xBB {
			t.Errorf("second NAL wrong: %x", nalus[1])
		}
	})

	t.Run("empty", func(t *testing.T) {
		nalus, key := splitAnnexB(nil)
		if len(nalus) != 0 || key {
			t.Errorf("empty AU should yield no NALs and not a keyframe")
		}
	})
}

// fakeSub is a test Subscription.
type fakeSub struct {
	frames chan source.AccessUnit
	closed atomic.Bool
}

func (f *fakeSub) Frames() <-chan source.AccessUnit { return f.frames }
func (f *fakeSub) Close()                           { f.closed.Store(true) }

func TestNewSource_RequiresSubscribe(t *testing.T) {
	if _, err := NewSource(Options{}); err == nil {
		t.Fatal("expected error when Subscribe is nil")
	}
}

func TestNewSource_Defaults(t *testing.T) {
	s, err := NewSource(Options{Subscribe: func() (Subscription, error) { return nil, nil }})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if s.gop != DefaultGOP || s.bitrateKbps != DefaultBitrateKbps || s.ffmpegPath != "ffmpeg" {
		t.Errorf("defaults not applied: gop=%d bitrate=%d ffmpeg=%q", s.gop, s.bitrateKbps, s.ffmpegPath)
	}
}

// TestStart_SubscribeError: a failed tap of the camera hub surfaces as a
// Start error and spawns no ffmpeg.
func TestStart_SubscribeError(t *testing.T) {
	wantErr := errors.New("hub down")
	s, err := NewSource(Options{
		Subscribe: func() (Subscription, error) { return nil, wantErr },
	})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if err := s.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "subscribe") {
		t.Fatalf("Start err = %v, want a subscribe error", err)
	}
}

// TestStart_FfmpegLaunchFailureClosesSubscription: if ffmpeg cannot launch,
// Start must release the camera subscription it already opened (no leak).
func TestStart_FfmpegLaunchFailureClosesSubscription(t *testing.T) {
	sub := &fakeSub{frames: make(chan source.AccessUnit)}
	s, err := NewSource(Options{
		Subscribe:  func() (Subscription, error) { return sub, nil },
		FFmpegPath: "definitely-not-a-real-ffmpeg-binary-xyz",
	})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("expected Start to fail when the ffmpeg binary is missing")
	}
	if !sub.closed.Load() {
		t.Error("subscription not closed after ffmpeg launch failure (leak)")
	}
}

// compile-time: *Source is a source.VideoSource (also asserted in the
// package, restated here so the test file documents the contract).
var _ source.VideoSource = (*Source)(nil)
