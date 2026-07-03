//go:build linux

// Local-shell backend for the edge host (Raspberry Pi). Opens an
// interactive shell on a PTY, running under the server's service user —
// deliberately so, admin-only: one click, no login. Linux-only (the edge
// target); other platforms get the stub in shell_other.go and the dock
// hides the local-shell button with a hint.
package console

import (
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
)

// LocalShellSupported reports whether a local shell can be opened on this
// host (true on the Linux edge build).
func LocalShellSupported() bool { return true }

// OpenLocalShell starts the service user's interactive shell on a fresh
// PTY and returns it as a Backend.
func OpenLocalShell() (Backend, error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell, "-i")
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	f, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	return &ptyBackend{cmd: cmd, f: f}, nil
}

// ptyBackend bridges the console frame to a local PTY-backed process.
type ptyBackend struct {
	cmd       *exec.Cmd
	f         *os.File
	closeOnce sync.Once
}

func (b *ptyBackend) Read(p []byte) (int, error)  { return b.f.Read(p) }
func (b *ptyBackend) Write(p []byte) (int, error) { return b.f.Write(p) }

func (b *ptyBackend) Resize(cols, rows uint16) error {
	return pty.Setsize(b.f, &pty.Winsize{Cols: cols, Rows: rows})
}

// Close reaps the child process and releases the PTY. Idempotent.
func (b *ptyBackend) Close() error {
	b.closeOnce.Do(func() {
		_ = b.f.Close()
		if b.cmd.Process != nil {
			_ = b.cmd.Process.Kill()
			// Reap in the background so Close never blocks on the child.
			go func() { _ = b.cmd.Wait() }()
		}
	})
	return nil
}
