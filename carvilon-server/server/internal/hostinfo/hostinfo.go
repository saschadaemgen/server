// Package hostinfo reports a human description of the host the server runs
// on - the Raspberry Pi model (if any), the Linux distribution, the kernel
// release and the architecture - for the editor's status bar. Pure stdlib
// and general Linux: it reads the device-tree model, /etc/os-release and
// the kernel osrelease, all plain file reads that are simply absent off
// Linux, where it falls back to the Go runtime's OS and arch. NOT
// RPi-hardcoded: a host is "a Pi" iff it has a device-tree model.
package hostinfo

import (
	"bufio"
	"os"
	"runtime"
	"strings"
)

// Info is the host description served to the editor's status bar.
type Info struct {
	Model  string `json:"model"`  // Pi model, "" when the host is not a Pi
	OS     string `json:"os"`     // distro PRETTY_NAME, or the Go GOOS off Linux
	Kernel string `json:"kernel"` // kernel release, "" when unknown
	Arch   string `json:"arch"`   // GOARCH
}

// Detect reads the host description. Cheap (a few small file reads); the
// caller may cache it, but the editor fetches it once per load anyway.
func Detect() Info {
	return Info{
		Model:  piModel(),
		OS:     osName(),
		Kernel: kernelRelease(),
		Arch:   runtime.GOARCH,
	}
}

// piModel returns the device-tree model string - a Pi (and many ARM SBCs)
// has one - or "" when absent (a VPS / mini-PC / the dev machine).
func piModel() string {
	for _, p := range []string{"/proc/device-tree/model", "/sys/firmware/devicetree/base/model"} {
		if b, err := os.ReadFile(p); err == nil {
			if m := cleanModel(b); m != "" {
				return m
			}
		}
	}
	return ""
}

// cleanModel strips the device-tree string's trailing NUL terminator and
// surrounding whitespace.
func cleanModel(b []byte) string {
	return strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", ""))
}

// osName returns the distro PRETTY_NAME, falling back to the capitalized Go
// GOOS off Linux (so the dev machine shows e.g. "Windows").
func osName() string {
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		if v := osReleaseField(b, "PRETTY_NAME"); v != "" {
			return v
		}
	}
	return capitalize(runtime.GOOS)
}

// osReleaseField extracts and unquotes a KEY=value field from os-release
// content (values are commonly double-quoted).
func osReleaseField(content []byte, key string) string {
	sc := bufio.NewScanner(strings.NewReader(string(content)))
	prefix := key + "="
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, prefix) {
			return strings.Trim(strings.TrimPrefix(line, prefix), `"`)
		}
	}
	return ""
}

// kernelRelease returns the running kernel release (Linux), or "".
func kernelRelease() string {
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

// capitalize upper-cases the first byte (ASCII GOOS values like "windows").
func capitalize(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
