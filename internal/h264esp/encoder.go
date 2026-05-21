package h264esp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"carvilon.local/stream/internal/source"
)

// Encoder wraps an ffmpeg subprocess that
//
//  1. accepts a raw H.264 Annex-B stream on stdin (camera AUs from the
//     shared per-camera hub, marshaled Annex-B),
//  2. decodes it,
//  3. scales / fps-resamples to the spec's target,
//  4. re-encodes as Constrained Baseline H.264 with in-band SPS/PPS
//     before every IDR (-bsf:v dump_extra=freq=keyframe),
//  5. writes the resulting Annex-B byte stream to stdout,
//
// and a goroutine inside this package runs an [AUSplitter] over that
// stdout to emit one whole Access Unit per output channel element.
//
// Identical lifecycle posture to [internal/mjpeg.Encoder]: Start
// spawns three I/O goroutines, AUs is a read-only output channel,
// Close terminates the subprocess and waits for clean exit. We do NOT
// auto-restart on crash — the calling Hub session tears itself down
// and viewers reconnect (which spawns a fresh encoder).
type Encoder struct {
	label      string // log-only, typically the profile name
	spec       EncodeSpec
	ffmpegPath string
	logger     *log.Logger

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	inputCh  chan source.AccessUnit
	outputCh chan []byte // emits one complete Annex-B AU per element

	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
	exitErr   error
}

// EncoderOptions configures an Encoder. Mirrors mjpeg.EncoderOptions
// so the call site shape stays familiar.
type EncoderOptions struct {
	Label      string // typically the profile name
	Spec       EncodeSpec
	FFmpegPath string // default: "ffmpeg"
	Logger     *log.Logger
	InputBuf   int // input channel size, default 8
	OutputBuf  int // output channel size, default 4
}

const (
	defaultInputBuf  = 8
	defaultOutputBuf = 4
)

// NewEncoder validates options and returns an un-started Encoder.
func NewEncoder(opts EncoderOptions) (*Encoder, error) {
	if err := opts.Spec.Validate(); err != nil {
		return nil, err
	}
	if opts.FFmpegPath == "" {
		opts.FFmpegPath = "ffmpeg"
	}
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	if opts.InputBuf <= 0 {
		opts.InputBuf = defaultInputBuf
	}
	if opts.OutputBuf <= 0 {
		opts.OutputBuf = defaultOutputBuf
	}
	if opts.Label == "" {
		opts.Label = "h264esp"
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Encoder{
		label:      opts.Label,
		spec:       opts.Spec,
		ffmpegPath: opts.FFmpegPath,
		logger:     opts.Logger,
		inputCh:    make(chan source.AccessUnit, opts.InputBuf),
		outputCh:   make(chan []byte, opts.OutputBuf),
		ctx:        ctx,
		cancel:     cancel,
	}, nil
}

// Input returns the channel the caller writes AccessUnits to. Use a
// non-blocking send so a slow ffmpeg cannot back-pressure into the
// shared camera hub (the briefing's hard rule).
func (e *Encoder) Input() chan<- source.AccessUnit { return e.inputCh }

// AUs returns the channel of encoded Access Units. Each element is a
// complete Annex-B byte slice ready to be written verbatim as one
// HTTP response chunk. Closed when the ffmpeg process exits.
func (e *Encoder) AUs() <-chan []byte { return e.outputCh }

// Label returns the configured log-label.
func (e *Encoder) Label() string { return e.label }

// Spec returns the encode configuration this instance was built with.
func (e *Encoder) Spec() EncodeSpec { return e.spec }

// Start spawns ffmpeg and the I/O goroutines.
func (e *Encoder) Start() error {
	args := buildFFmpegArgs(e.spec)

	e.cmd = exec.CommandContext(e.ctx, e.ffmpegPath, args...)

	var err error
	if e.stdin, err = e.cmd.StdinPipe(); err != nil {
		return fmt.Errorf("h264esp: stdin pipe: %w", err)
	}
	if e.stdout, err = e.cmd.StdoutPipe(); err != nil {
		return fmt.Errorf("h264esp: stdout pipe: %w", err)
	}
	if e.stderr, err = e.cmd.StderrPipe(); err != nil {
		return fmt.Errorf("h264esp: stderr pipe: %w", err)
	}

	if err := e.cmd.Start(); err != nil {
		return fmt.Errorf("h264esp: ffmpeg start (%s): %w", e.ffmpegPath, err)
	}
	e.logger.Printf("h264esp: ffmpeg started for %q (pid=%d, %dx%d @ %d fps CRF %d, GoP %d)",
		e.label, e.cmd.Process.Pid, e.spec.Width, e.spec.Height, e.spec.FPS, e.spec.Quality, e.spec.FPS)

	e.wg.Add(3)
	go e.runStdin()
	go e.runStdout()
	go e.runStderr()
	return nil
}

// buildFFmpegArgs assembles the full ffmpeg argv. Input side reads raw
// H.264 Annex-B from stdin (which is what runStdin writes after
// prefixing each NAL with `00 00 00 01`).
func buildFFmpegArgs(s EncodeSpec) []string {
	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-nostats",
		"-fflags", "+nobuffer",
		// Input: raw H.264 Annex-B on stdin.
		"-f", "h264",
		"-i", "pipe:0",
	}
	args = append(args, s.OutputArgs()...)
	args = append(args, "pipe:1")
	return args
}

func (e *Encoder) runStdin() {
	defer e.wg.Done()
	defer func() { _ = e.stdin.Close() }()

	startCodes := []byte{0x00, 0x00, 0x00, 0x01}
	for {
		select {
		case <-e.ctx.Done():
			return
		case au, ok := <-e.inputCh:
			if !ok {
				return
			}
			ok2 := true
			for _, nalu := range au.NALUs {
				if _, err := e.stdin.Write(startCodes); err != nil {
					e.logger.Printf("h264esp: stdin write (%q): %v", e.label, err)
					e.cancel()
					ok2 = false
					break
				}
				if _, err := e.stdin.Write(nalu); err != nil {
					e.logger.Printf("h264esp: stdin write (%q): %v", e.label, err)
					e.cancel()
					ok2 = false
					break
				}
			}
			if !ok2 {
				return
			}
		}
	}
}

func (e *Encoder) runStdout() {
	defer e.wg.Done()
	defer close(e.outputCh)

	sp := NewAUSplitter(e.stdout)
	for {
		au, err := sp.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
				e.logger.Printf("h264esp: stdout read (%q): %v", e.label, err)
			}
			return
		}
		select {
		case e.outputCh <- au:
		case <-e.ctx.Done():
			return
		}
	}
}

// runStderr drains ffmpeg's stderr so the pipe never fills. With
// -loglevel error the channel is mostly silent; when something IS
// logged we forward it so a real failure is visible.
func (e *Encoder) runStderr() {
	defer e.wg.Done()

	buf := make([]byte, 4096)
	for {
		n, err := e.stderr.Read(buf)
		if n > 0 {
			msg := strings.TrimRight(string(buf[:n]), "\r\n ")
			if msg != "" {
				e.logger.Printf("h264esp: ffmpeg stderr (%q): %s", e.label, msg)
			}
		}
		if err != nil {
			return
		}
	}
}

// Close terminates ffmpeg and waits for all goroutines. Idempotent.
func (e *Encoder) Close() error {
	e.closeOnce.Do(func() {
		e.cancel()
		if e.cmd != nil && e.cmd.Process != nil {
			_ = e.cmd.Process.Kill()
		}
		doneCh := make(chan struct{})
		go func() {
			e.wg.Wait()
			close(doneCh)
		}()
		select {
		case <-doneCh:
		case <-time.After(5 * time.Second):
			e.logger.Printf("h264esp: encoder %q goroutines did not exit within 5s", e.label)
		}
		if e.cmd != nil {
			e.exitErr = e.cmd.Wait()
		}
	})
	return e.exitErr
}
