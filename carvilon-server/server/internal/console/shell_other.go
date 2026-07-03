//go:build !linux

// Local-shell stub for non-Linux hosts (dev Windows/macOS). The edge
// target is Linux; there is no local shell here. The outbound SSH client
// still works on every platform. The dock queries LocalShellSupported()
// and hides the local-shell button with a hint when it is false.
package console

// LocalShellSupported reports false off the Linux edge build.
func LocalShellSupported() bool { return false }

// OpenLocalShell always fails off Linux (ErrLocalShellUnsupported is
// defined in session.go so callers compile on every platform).
func OpenLocalShell() (Backend, error) { return nil, ErrLocalShellUnsupported }
