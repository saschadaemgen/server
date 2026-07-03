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
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Info is the host description served to the editor's status bar.
type Info struct {
	Model  string `json:"model"`  // Pi model, "" when the host is not a Pi
	OS     string `json:"os"`     // distro PRETTY_NAME, or the Go GOOS off Linux
	Kernel string `json:"kernel"` // kernel release, "" when unknown
	Arch   string `json:"arch"`   // GOARCH
	RAM    string `json:"ram"`    // total RAM as a friendly size ("8 GB"), "" when unknown
}

// Detect reads the host description. Cheap (a few small file reads); the
// caller may cache it, but the editor fetches it once per load anyway.
func Detect() Info {
	return Info{
		Model:  piModel(),
		OS:     osName(),
		Kernel: kernelRelease(),
		Arch:   runtime.GOARCH,
		RAM:    ramTotal(),
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

// ramTotal returns the total RAM as a friendly size string ("8 GB"), read
// from /proc/meminfo MemTotal, or "" when unavailable (off Linux).
func ramTotal() string {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return ""
	}
	return formatRAM(memTotalKB(b))
}

// memTotalKB extracts the MemTotal value (in kB) from /proc/meminfo content,
// or 0 when the field is absent or unparseable. The line looks like
// "MemTotal:        8052812 kB".
func memTotalKB(content []byte) int64 {
	sc := bufio.NewScanner(strings.NewReader(string(content)))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			return 0
		}
		kb, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil || kb < 0 {
			return 0
		}
		return kb
	}
	return 0
}

// formatRAM renders a MemTotal (kB) as a friendly size. Reported RAM sits a
// little below the nominal capacity (firmware/GPU reserve), so >=1 GiB is
// rounded to the nearest whole GB — recovering the marketing size (1/2/4/8)
// for Pi-class hosts; smaller amounts fall back to MB. 0 yields "".
func formatRAM(kb int64) string {
	if kb <= 0 {
		return ""
	}
	if gib := float64(kb) / (1024 * 1024); gib >= 1 {
		return strconv.FormatInt(int64(math.Round(gib)), 10) + " GB"
	}
	mib := float64(kb) / 1024
	return strconv.FormatInt(int64(math.Round(mib)), 10) + " MB"
}

// capitalize upper-cases the first byte (ASCII GOOS values like "windows").
func capitalize(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
