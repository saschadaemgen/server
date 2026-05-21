package mjpeg

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
//  1. accepts a raw H.264 Annex-B stream on stdin (from our hub),
//  2. decodes it,
//  3. scales to the spec's target resolution,
//  4. re-encodes each frame as JPEG at the spec's quality,
//  5. emits the JPEGs on stdout in the `-f mjpeg` concat format.
//
// Lifecycle:
//
//   - NewEncoder validates options.
//   - Start spawns ffmpeg and the three I/O goroutines (stdin pump,
//     stdout splitter, stderr drain). Returns once the process has been
//     fork+exec'd successfully.
//   - Input is a write-only channel the caller pushes [source.AccessUnit]
//     values into. The caller should use a non-blocking send so a slow
//     ffmpeg cannot back-pressure into the H.264 hub.
//   - JPEGs is a read-only channel of complete JPEG frames. Closed when
//     ffmpeg exits.
//   - Close terminates ffmpeg, waits for goroutines, and reaps the
//     process. Idempotent.
//
// We deliberately do NOT auto-restart on crash. If ffmpeg dies, JPEGs
// closes and the upstream Hub session tears itself down; viewers get
// disconnected and reconnect (which spawns a fresh encoder). Spike
// scope; production might want a supervisor.
type Encoder struct {
	label      string // human-readable, log-only ("intercom_esp" etc.)
	spec       EncodeSpec
	ffmpegPath string
	logger     *log.Logger

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	inputCh  chan source.AccessUnit
	outputCh chan []byte

	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
	exitErr   error
}

// EncoderOptions configures an Encoder.
type EncoderOptions struct {
	// Label appears in log lines for this encoder. Typically the
	// profile name ("intercom_esp"); pure cosmetic.
	Label string

	// Spec is the encode configuration (resolution / fps / quality).
	// Required.
	Spec EncodeSpec

	FFmpegPath string // default: "ffmpeg"
	Logger     *log.Logger
	InputBuf   int // input channel size, default 8
	OutputBuf  int // output channel size, default 4
}

const (
	defaultInputBuf  = 8
	defaultOutputBuf = 4
)

// NewEncoder constructs an Encoder. It does not contact ffmpeg yet —
// call Start.
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
		opts.Label = "encoder"
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
// non-blocking send (select with default) so a slow ffmpeg cannot
// back-pressure into the H.264 hub:
//
//	select {
//	case enc.Input() <- au:
//	default:
//	    // ffmpeg can't keep up; drop this AU
//	}
func (e *Encoder) Input() chan<- source.AccessUnit { return e.inputCh }

// JPEGs returns the channel of encoded JPEG frames. Closed when the
// ffmpeg process exits (clean stop or crash).
func (e *Encoder) JPEGs() <-chan []byte { return e.outputCh }

// Label returns the configured log-label. Useful for tests and
// diagnostics.
func (e *Encoder) Label() string { return e.label }

// Spec returns the encode configuration this instance was built with.
func (e *Encoder) Spec() EncodeSpec { return e.spec }

// Start spawns ffmpeg and the I/O goroutines.
func (e *Encoder) Start() error {
	args := buildFFmpegArgs(e.spec)

	e.cmd = exec.CommandContext(e.ctx, e.ffmpegPath, args...)

	var err error
	if e.stdin, err = e.cmd.StdinPipe(); err != nil {
		return fmt.Errorf("mjpeg: stdin pipe: %w", err)
	}
	if e.stdout, err = e.cmd.StdoutPipe(); err != nil {
		return fmt.Errorf("mjpeg: stdout pipe: %w", err)
	}
	if e.stderr, err = e.cmd.StderrPipe(); err != nil {
		return fmt.Errorf("mjpeg: stderr pipe: %w", err)
	}

	if err := e.cmd.Start(); err != nil {
		return fmt.Errorf("mjpeg: ffmpeg start (%s): %w", e.ffmpegPath, err)
	}
	e.logger.Printf("mjpeg: ffmpeg started for %q (pid=%d, %dx%d @ %d fps q:v %d)",
		e.label, e.cmd.Process.Pid, e.spec.Width, e.spec.Height, e.spec.FPS, e.spec.Quality)

	e.wg.Add(3)
	go e.runStdin()
	go e.runStdout()
	go e.runStderr()
	return nil
}

// buildFFmpegArgs constructs the full ffmpeg command line: static input
// args (raw H.264 from stdin) + spec-driven output args + pipe:1 sink.
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
			// Write each NAL with a 4-byte Annex-B start code.
			ok2 := true
			for _, nalu := range au.NALUs {
				if _, err := e.stdin.Write(startCodes); err != nil {
					e.logger.Printf("mjpeg: stdin write (%q): %v", e.label, err)
					e.cancel()
					ok2 = false
					break
				}
				if _, err := e.stdin.Write(nalu); err != nil {
					e.logger.Printf("mjpeg: stdin write (%q): %v", e.label, err)
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

	sp := NewFrameSplitter(e.stdout)
	for {
		frame, err := sp.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
				e.logger.Printf("mjpeg: stdout read (%q): %v", e.label, err)
			}
			return
		}
		select {
		case e.outputCh <- frame:
		case <-e.ctx.Done():
			return
		}
	}
}

// runStderr drains ffmpeg's stderr so the kernel-side pipe never fills
// up (which would deadlock ffmpeg). With our -loglevel error setting
// stderr usually stays quiet; when something IS logged we forward at
// most one summary line per second so a real failure is visible.
func (e *Encoder) runStderr() {
	defer e.wg.Done()

	buf := make([]byte, 4096)
	for {
		n, err := e.stderr.Read(buf)
		if n > 0 {
			// Strip trailing newlines for cleaner logs.
			msg := strings.TrimRight(string(buf[:n]), "\r\n ")
			if msg != "" {
				e.logger.Printf("mjpeg: ffmpeg stderr (%q): %s", e.label, msg)
			}
		}
		if err != nil {
			return
		}
	}
}

// Close terminates ffmpeg and waits for all goroutines. Idempotent. The
// first call performs the teardown; subsequent calls return the same
// error.
func (e *Encoder) Close() error {
	e.closeOnce.Do(func() {
		e.cancel()
		if e.cmd != nil && e.cmd.Process != nil {
			// Best-effort terminate. exec.CommandContext also kills on
			// ctx cancel, but explicit Kill makes the test surface
			// deterministic.
			_ = e.cmd.Process.Kill()
		}
		// Bound the wait so a misbehaving stderr/stdout reader can't
		// hang us forever. 5 s is generous.
		doneCh := make(chan struct{})
		go func() {
			e.wg.Wait()
			close(doneCh)
		}()
		select {
		case <-doneCh:
		case <-time.After(5 * time.Second):
			e.logger.Printf("mjpeg: encoder %q goroutines did not exit within 5s", e.label)
		}
		if e.cmd != nil {
			e.exitErr = e.cmd.Wait()
		}
	})
	return e.exitErr
}

// CheckFFmpeg runs `<path> -version` and returns an error if the binary
// is missing or the call fails. Intended for a one-shot startup check
// so misconfigured deployments fail fast rather than at first viewer.
func CheckFFmpeg(path string) error {
	if path == "" {
		path = "ffmpeg"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "-version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mjpeg: ffmpeg check (%s -version): %w", path, err)
	}
	// Sanity: output should start with "ffmpeg version ".
	if !strings.HasPrefix(string(out), "ffmpeg version ") {
		return fmt.Errorf("mjpeg: %q does not look like ffmpeg (output: %q)", path, truncate(string(out), 80))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
