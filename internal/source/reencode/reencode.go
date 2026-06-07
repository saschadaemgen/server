// Package reencode is the S4 4G/cloud source pipeline: a short-GOP
// RE-ENCODE of an existing camera stream.
//
// # Why it exists
//
// The UniFi camera emits H.264 with a long GOP (~105 frames). On a clean
// LAN that is fine — a dropped packet is repaired by NACK/RTX before the
// next reference frame. Over a lossy 4G uplink to the cloud, a lost packet
// that cannot be retransmitted in time corrupts every frame until the NEXT
// keyframe, i.e. the viewer FREEZES for up to a whole GOP (several seconds).
//
// This source re-encodes the camera stream with a SMALL keyframe interval
// (-g ~20..25), so a 4G viewer recovers within a fraction of a second.
// FlexFEC + NACK on the cloud egress then ride on top of the now-clean
// short-GOP stream. The LAN-high passthrough path is untouched — this is a
// SEPARATE pipeline, selected per profile via
// [profile.CodecH264ReencodeShortGOP].
//
// # One camera pull
//
// The re-encode does NOT open a second connection to the camera. It taps
// the SHARED camera passthrough hub through an injected [Options.Subscribe]
// closure: the registry hands it a subscription to the same (camera, tier)
// pull every other consumer uses. So the topology is exactly one camera
// pull -> one re-encode ffmpeg -> the cloud-push fan-out. (Injecting the
// subscription instead of importing the registry also keeps this package
// free of an import cycle and trivially testable with a fake hub.)
//
// # Edge encode
//
// The encode is libx264 (software) with software multi-core DECODE
// (-threads 4). It was h264_v4l2m2m (RPi HW) until the cloud/4G field test
// showed the real lever is a tight VBV window (-maxrate/-bufsize), which the
// v4l2m2m wrapper does not expose (only -b:v; confirmed on the RPi:
// `ffmpeg -h encoder=h264_v4l2m2m` lists only buffer counts). VBV presses the
// periodic IDR into a small budget (a smaller, blockier keyframe instead of a
// big packet burst) and keeps the bitrate even, which is what a lossy radio
// leg needs.
//
// We keep PERIODIC IDRs (-g), NOT intra-refresh: a WebRTC receiver repairs
// loss by sending a PLI and WAITING for an IDR; an intra-refresh stream emits
// no IDR, so the decoder drops everything and renders a black screen (GDR is
// H.266-only, not in the H.264 WebRTC path). SPS/PPS ride in-band before
// every IDR (dump_extra=freq=keyframe), as before.
//
// The output is raw Annex-B on stdout, parsed back into access units by the
// same [h264esp.AUSplitter] the CBP path uses. That splitter requires ONE VCL
// NAL per access unit, so -tune zerolatency's implicit sliced-threads=1 is
// overridden back to single-slice (sliced-threads=0:slices=1), exactly as the
// CBP path does (S6-04) - otherwise each slice would surface as a bogus
// separate frame.
//
// Lifecycle posture mirrors the other ffmpeg subprocess wrappers
// ([internal/h264esp.Encoder], [internal/mjpeg.Encoder]): Start spawns
// three I/O goroutines, Frames() is the read-only output, Close() kills the
// subprocess and waits. No auto-restart: when the camera feed ends or the
// last cloud viewer leaves, the hub tears this source down and a later
// viewer rebuilds it.
package reencode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"carvilon.local/stream/internal/droplog"
	"carvilon.local/stream/internal/h264esp"
	"carvilon.local/stream/internal/source"
)

// Defaults for the short-GOP re-encode. Both are deployment knobs the
// caller may override via [Options]; they are intentionally conservative
// for a 4G uplink and are flagged RPi-verify (the real numbers come from
// measuring on the Pi against a live 4G link).
const (
	// DefaultGOP is the keyframe interval in frames. ~20..25 keeps the
	// freeze-on-loss window under a second at the camera's medium fps while
	// not exploding the bitrate. Periodic IDRs (NOT intra-refresh) so WebRTC
	// PLI repair works.
	DefaultGOP = 25
	// DefaultBitrateKbps is the libx264 VBV target bitrate (kbit/s). 800 kbit/s
	// after the cloud/4G field test: smaller frames mean smaller per-frame
	// packet bursts (gentler on a lossy radio leg) and leave headroom for the
	// raised ~50% FlexFEC overhead on the egress. -b:v and -maxrate are both
	// set to this. Iterative knob, 600-1000k band.
	DefaultBitrateKbps = 800
	// DefaultBufsizeKbps is the libx264 VBV buffer (kbit). This is THE lever
	// the switch from v4l2m2m bought: a tight VBV window spreads the periodic
	// IDR over the budget instead of one big burst. ~400 kbit ~= 0.5s at the
	// 800k target; SMALLER = smoother on the wire but a blockier IDR. Start
	// 400, tune down if the wire still bursts.
	DefaultBufsizeKbps = 400
)

const (
	// framesBuffer is the depth of the output Frames() channel. Tiny on
	// purpose (drop-statt-buffer): a slow downstream loses a frame rather
	// than backing latency up into the encoder. Matches the unifi source.
	framesBuffer = 4
	// clockRate90kHz is the H.264 RTP clock. Output PTS is synthesised in
	// these units from wall-clock arrival (see runStdout).
	clockRate90kHz = 90000
	// goroutineDrainTimeout bounds Close()'s wait for the I/O goroutines.
	goroutineDrainTimeout = 5 * time.Second
)

// Subscription is the minimal view this package needs of a camera
// passthrough subscription. *hub.Subscriber satisfies it; defining it as an
// interface keeps reencode decoupled from internal/hub and internal/sourcereg
// (no import cycle) and lets tests inject a fake.
type Subscription interface {
	// Frames is the stream of raw camera access units to re-encode. Closed
	// by the upstream hub when the camera feed ends or this subscription is
	// Closed.
	Frames() <-chan source.AccessUnit
	// Close releases the subscription. When this was the last consumer of
	// the camera pull, the pull stops.
	Close()
}

// Options configures a re-encode [Source].
type Options struct {
	// Subscribe taps the shared camera passthrough hub. Required. Called
	// once, at Start. The returned subscription's Frames() carries the raw
	// camera H.264 that this source re-encodes; closing the Source closes
	// the subscription. Injecting it (rather than importing the registry)
	// is what guarantees ONE camera pull and keeps this package decoupled.
	Subscribe func() (Subscription, error)

	// FFmpegPath is the ffmpeg binary. Default "ffmpeg" (resolved via PATH).
	FFmpegPath string

	// GOP is the keyframe interval in frames. 0 -> [DefaultGOP].
	GOP int

	// BitrateKbps is the libx264 VBV target bitrate in kbit/s (-b:v and
	// -maxrate). 0 -> [DefaultBitrateKbps].
	BitrateKbps int

	// BufsizeKbps is the libx264 VBV buffer in kbit (-bufsize) - the lever
	// that spreads the IDR burst. 0 -> [DefaultBufsizeKbps].
	BufsizeKbps int

	// Logger receives diagnostic output. nil -> the default logger.
	Logger *log.Logger

	// now overrides the clock used to synthesise output PTS. nil ->
	// time.Now. Test seam only.
	now func() time.Time
}

// Source re-encodes a tapped camera stream to short-GOP H.264 and exposes
// the result through [source.VideoSource].
type Source struct {
	subscribe   func() (Subscription, error)
	ffmpegPath  string
	gop         int
	bitrateKbps int
	bufsizeKbps int
	logger      *log.Logger
	now         func() time.Time

	sub    Subscription
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	framesCh chan source.AccessUnit
	drops    *droplog.Counter

	ctx       context.Context
	cancel    context.CancelFunc
	startTime time.Time
	wg        sync.WaitGroup
	closeOnce sync.Once
	exitErr   error
}

// Compile-time interface satisfaction check.
var _ source.VideoSource = (*Source)(nil)

// NewSource validates options and returns an un-started Source. It does not
// subscribe or spawn ffmpeg — call Start.
func NewSource(opts Options) (*Source, error) {
	if opts.Subscribe == nil {
		return nil, errors.New("reencode: Subscribe is required")
	}
	if opts.FFmpegPath == "" {
		opts.FFmpegPath = "ffmpeg"
	}
	if opts.GOP <= 0 {
		opts.GOP = DefaultGOP
	}
	if opts.BitrateKbps <= 0 {
		opts.BitrateKbps = DefaultBitrateKbps
	}
	if opts.BufsizeKbps <= 0 {
		opts.BufsizeKbps = DefaultBufsizeKbps
	}
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	return &Source{
		subscribe:   opts.Subscribe,
		ffmpegPath:  opts.FFmpegPath,
		gop:         opts.GOP,
		bitrateKbps: opts.BitrateKbps,
		bufsizeKbps: opts.BufsizeKbps,
		logger:      opts.Logger,
		now:         opts.now,
		framesCh:    make(chan source.AccessUnit, framesBuffer),
		drops:       &droplog.Counter{Logger: opts.Logger, Label: "reencode: frames channel full"},
	}, nil
}

// Start taps the camera passthrough hub and spawns the re-encode ffmpeg.
//
// It does NOT block until access units flow (the encode has startup
// latency); the hub tolerates this by waiting on Frames(). Start returns an
// error if the subscription cannot be opened or ffmpeg fails to launch — in
// either failure path it releases anything it already acquired.
func (s *Source) Start(ctx context.Context) error {
	// Derive our own cancellable context from the caller's (the hub's), so
	// both hub shutdown AND Close() stop the subprocess and goroutines.
	s.ctx, s.cancel = context.WithCancel(ctx)

	sub, err := s.subscribe()
	if err != nil {
		s.cancel()
		return fmt.Errorf("reencode: subscribe to camera passthrough hub: %w", err)
	}
	s.sub = sub

	args := buildFFmpegArgs(s.gop, s.bitrateKbps, s.bufsizeKbps)
	s.cmd = exec.CommandContext(s.ctx, s.ffmpegPath, args...)

	if s.stdin, err = s.cmd.StdinPipe(); err != nil {
		s.abortStart()
		return fmt.Errorf("reencode: stdin pipe: %w", err)
	}
	if s.stdout, err = s.cmd.StdoutPipe(); err != nil {
		s.abortStart()
		return fmt.Errorf("reencode: stdout pipe: %w", err)
	}
	if s.stderr, err = s.cmd.StderrPipe(); err != nil {
		s.abortStart()
		return fmt.Errorf("reencode: stderr pipe: %w", err)
	}
	if err := s.cmd.Start(); err != nil {
		s.abortStart()
		return fmt.Errorf("reencode: ffmpeg start (%s): %w", s.ffmpegPath, err)
	}
	s.startTime = s.now()
	s.logger.Printf("reencode: ffmpeg started (pid=%d, libx264 VBV, -g %d, %d kbit/s, bufsize %d kbit)",
		s.cmd.Process.Pid, s.gop, s.bitrateKbps, s.bufsizeKbps)

	s.wg.Add(3)
	go s.runStdin()
	go s.runStdout()
	go s.runStderr()
	return nil
}

// abortStart releases the subscription, closes any pipe already opened, and
// cancels the context after a failure during Start, before any goroutine was
// launched. Closing the pipes prevents an FD leak when ffmpeg fails to spawn.
func (s *Source) abortStart() {
	s.cancel()
	for _, c := range []io.Closer{s.stdin, s.stdout, s.stderr} {
		if c != nil {
			_ = c.Close()
		}
	}
	if s.sub != nil {
		s.sub.Close()
		s.sub = nil
	}
}

// Frames returns the re-encoded access-unit stream. Closed exactly once
// (by runStdout) when ffmpeg exits — that is the kernel's end-of-stream
// signal.
func (s *Source) Frames() <-chan source.AccessUnit { return s.framesCh }

// Params returns empty H.264 params. The re-encoded SPS/PPS are repeated
// IN-BAND before every IDR (-bsf:v dump_extra=freq=keyframe), so the WebRTC
// passthrough egress does not need them out-of-band, and synthesising a
// ProfileLevelID here would only risk disagreeing with the wire. Empty is
// explicitly permitted by [source.H264Params].
func (s *Source) Params() source.H264Params { return source.H264Params{} }

// runStdin pumps tapped camera access units into ffmpeg's stdin as Annex-B.
// Writes are blocking: back-pressure is absorbed by the hub, which drops to
// THIS subscription's channel rather than into the camera pull. When the
// camera feed ends (Frames closed) it closes stdin so ffmpeg drains and
// exits.
func (s *Source) runStdin() {
	defer s.wg.Done()
	defer func() { _ = s.stdin.Close() }()

	startCode := []byte{0x00, 0x00, 0x00, 0x01}
	for {
		select {
		case <-s.ctx.Done():
			return
		case au, ok := <-s.sub.Frames():
			if !ok {
				return // upstream camera feed ended
			}
			for _, nalu := range au.NALUs {
				if _, err := s.stdin.Write(startCode); err != nil {
					s.failStdin(err)
					return
				}
				if _, err := s.stdin.Write(nalu); err != nil {
					s.failStdin(err)
					return
				}
			}
		}
	}
}

func (s *Source) failStdin(err error) {
	// A broken stdin pipe after Close()/Kill is expected; only log when we
	// were not already tearing down.
	if s.ctx.Err() == nil {
		s.logger.Printf("reencode: stdin write: %v", err)
	}
	s.cancel()
}

// runStdout reads ffmpeg's Annex-B output, splits it into access units, and
// emits each as a [source.AccessUnit]. PTS is synthesised from wall-clock
// arrival in 90 kHz units: the re-encode boundary does not preserve a media
// clock on the raw-Annex-B output, and downstream ([feedTrack]) consumes
// only the PTS DELTA for sample duration (and clamps it), so wall-clock
// arrival is a robust, self-correcting substitute. VIDEO-ONLY: there is no
// audio to sync against.
func (s *Source) runStdout() {
	defer s.wg.Done()
	defer close(s.framesCh)

	sp := h264esp.NewAUSplitter(s.stdout)
	for {
		annexB, err := sp.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) && s.ctx.Err() == nil {
				s.logger.Printf("reencode: stdout read: %v", err)
			}
			return
		}
		nalus, isKeyframe := splitAnnexB(annexB)
		if len(nalus) == 0 {
			continue
		}
		d := s.now().Sub(s.startTime)
		if d < 0 {
			d = 0
		}
		au := source.AccessUnit{
			NALUs:      nalus,
			PTS:        d.Milliseconds() * clockRate90kHz / 1000,
			IsKeyframe: isKeyframe,
		}
		// Non-blocking send (drop-statt-buffer): a slow downstream loses
		// this frame; ffmpeg and the camera pull are unaffected.
		select {
		case s.framesCh <- au:
		default:
			s.drops.Record(errors.New("frames channel full"))
		}
	}
}

// runStderr drains ffmpeg's stderr so the pipe never fills, forwarding any
// real error (loglevel is "error", so it is mostly silent).
func (s *Source) runStderr() {
	defer s.wg.Done()

	buf := make([]byte, 4096)
	for {
		n, err := s.stderr.Read(buf)
		if n > 0 {
			msg := strings.TrimRight(string(buf[:n]), "\r\n ")
			if msg != "" {
				s.logger.Printf("reencode: ffmpeg stderr: %s", msg)
			}
		}
		if err != nil {
			return
		}
	}
}

// Close terminates ffmpeg, releases the camera subscription, and waits for
// the I/O goroutines. Idempotent.
func (s *Source) Close() error {
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		if s.sub != nil {
			s.sub.Close() // may stop the camera pull if we were the last consumer
		}
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		done := make(chan struct{})
		go func() {
			s.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(goroutineDrainTimeout):
			s.logger.Printf("reencode: goroutines did not exit within %s", goroutineDrainTimeout)
		}
		if s.cmd != nil {
			s.exitErr = s.cmd.Wait()
		}
	})
	return s.exitErr
}

// buildFFmpegArgs assembles the full ffmpeg argv for the short-GOP edge
// re-encode. Pure (no I/O) so it is unit-testable — the test pins the
// approved decisions (libx264, VBV -maxrate/-bufsize, single-slice, -g, -bf 0,
// NOT intra-refresh, NOT -force_key_frames, NOT v4l2m2m, video-only,
// SW-threaded decode).
//
// Input side mirrors internal/h264esp (the proven stdin-H264 handling):
//
//   - -fflags +nobuffer: format-level low delay.
//   - NO -flags +low_delay: it disables ffmpeg multi-core frame threading
//     and starves the decode (S2-16/D-0002, Canary). Deliberately absent.
//   - -use_wallclock_as_timestamps 1: the -f h264 demuxer otherwise fakes
//     PTS at 25 fps; we want the real cadence to reach the encoder. With
//     libx264 VBV the input cadence matters a touch more than it did on
//     v4l2m2m (it feeds the rate model); a suspect if the wire looks jittery,
//     but runStdout re-stamps output PTS so the leverage is low (B4).
//   - -threads 4: software multi-core DECODE of the medium input.
//
// Output side (Stufe C: libx264 SW with a tight VBV window):
//
//   - -an: VIDEO-ONLY (the 4G path ignores audio entirely).
//   - -c:v libx264 -preset ultrafast -tune zerolatency: low CPU + low latency.
//   - -x264-params sliced-threads=0:slices=1: -tune zerolatency implicitly
//     turns on sliced-threads=1 (multi-slice per frame); [h264esp.AUSplitter]
//     needs ONE VCL NAL per access unit, so force single-slice or each slice
//     surfaces as a bogus separate frame (S6-04, same fix as the CBP path).
//   - -b:v / -maxrate <kbit>k + -bufsize <kbit>k: the VBV window. THIS is the
//     lever the switch away from v4l2m2m bought (the v4l2m2m wrapper exposes
//     no VBV): it presses the periodic IDR into a small budget — a smaller,
//     blockier keyframe instead of a big packet burst — and keeps the bitrate
//     even, which is what a lossy radio leg needs.
//   - -g <gop>: PERIODIC IDRs (NOT intra-refresh). A WebRTC receiver repairs
//     loss by PLI + waiting for an IDR; intra-refresh emits none -> black
//     screen (GDR is H.266-only). Periodic IDRs keep PLI working.
//   - -bf 0: no B-frames (lowest latency).
//   - -bsf:v dump_extra=freq=keyframe: repeat SPS+PPS in-band before every
//     IDR so a (re)joining viewer decodes from the first keyframe it sees.
//   - -f h264: raw Annex-B byte stream to stdout (NO container), parsed by
//     [h264esp.AUSplitter].
//
// NO -profile:v: libx264's default profile (High for this content) is what the
// camera + passthrough already deliver and Android decodes; -profile:v
// high/main is a fallback only if a subscriber balks.
func buildFFmpegArgs(gop, bitrateKbps, bufsizeKbps int) []string {
	rate := strconv.Itoa(bitrateKbps) + "k"
	return []string{
		"-hide_banner",
		"-loglevel", "error",
		"-nostats",
		"-fflags", "+nobuffer",
		"-use_wallclock_as_timestamps", "1",
		"-threads", "4",
		"-f", "h264",
		"-i", "pipe:0",
		"-an",
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-tune", "zerolatency",
		// S6-04: undo zerolatency's sliced-threads=1; one VCL NAL per AU.
		"-x264-params", "sliced-threads=0:slices=1",
		"-b:v", rate,
		"-maxrate", rate,
		"-bufsize", strconv.Itoa(bufsizeKbps) + "k",
		"-g", strconv.Itoa(gop),
		"-bf", "0",
		"-bsf:v", "dump_extra=freq=keyframe",
		"-f", "h264",
		"pipe:1",
	}
}

// splitAnnexB splits one Annex-B access unit (NALs prefixed with 3- or
// 4-byte start codes, as produced by [h264esp.AUSplitter]) into raw NAL
// byte slices WITHOUT start codes, and reports whether any NAL is an IDR
// slice (type 5) — i.e. whether the AU is a keyframe.
//
// The returned slices alias au's backing array; the caller owns au (each
// AUSplitter.Next returns a fresh buffer), so this is safe and copy-free.
// Pure: unit-testable without ffmpeg.
func splitAnnexB(au []byte) (nalus [][]byte, isKeyframe bool) {
	i := 0
	for i < len(au) {
		sc := startCodeLen(au, i)
		if sc == 0 {
			next := indexStartCode(au, i+1)
			if next < 0 {
				break
			}
			i = next
			continue
		}
		payload := i + sc
		end := indexStartCode(au, payload)
		if end < 0 {
			end = len(au)
		}
		if payload < end {
			nal := au[payload:end]
			nalus = append(nalus, nal)
			if nal[0]&0x1F == nalUnitTypeIDR {
				isKeyframe = true
			}
		}
		i = end
	}
	return nalus, isKeyframe
}

const nalUnitTypeIDR = 5

// startCodeLen reports the Annex-B start-code length (3 or 4) at buf[i], or
// 0 if buf[i] is not the first byte of a start code.
func startCodeLen(buf []byte, i int) int {
	if i+3 < len(buf) && buf[i] == 0 && buf[i+1] == 0 && buf[i+2] == 0 && buf[i+3] == 1 {
		return 4
	}
	if i+2 < len(buf) && buf[i] == 0 && buf[i+1] == 0 && buf[i+2] == 1 {
		return 3
	}
	return 0
}

// indexStartCode returns the index of the next Annex-B start code in
// buf at or after from, or -1 if none.
func indexStartCode(buf []byte, from int) int {
	for i := from; i+2 < len(buf); i++ {
		if buf[i] != 0 || buf[i+1] != 0 {
			continue
		}
		if buf[i+2] == 1 {
			return i
		}
		if buf[i+2] == 0 && i+3 < len(buf) && buf[i+3] == 1 {
			return i
		}
	}
	return -1
}
