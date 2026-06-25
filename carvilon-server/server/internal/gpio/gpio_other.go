//go:build !linux

package gpio

import "errors"

// platformProbe always reports no GPIO off Linux: the character-device
// interface is Linux-only, so the catalog/run GPIO surface never appears
// on the dev machine (Windows/macOS) or any non-Linux host.
func platformProbe() (Status, []string) { return Unavailable, nil }

// platformLines has no lines to enumerate off Linux.
func platformLines([]string) []LineInfo { return nil }

// newDriver is unreachable off Linux (Available() is false there), but
// must exist for the package to compile; it errors loudly if ever called.
func newDriver([]string) (IODriver, error) {
	return nil, errors.New("gpio: not supported on this platform")
}
