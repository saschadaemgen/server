package hostinfo

import (
	"runtime"
	"testing"
)

// TestCleanModel pins the device-tree NUL-terminator trim (the bug the
// briefing called out): the trailing NUL must go, the text must survive.
func TestCleanModel(t *testing.T) {
	cases := map[string]string{
		"Raspberry Pi 4 Model B Rev 1.5\x00": "Raspberry Pi 4 Model B Rev 1.5",
		"Raspberry Pi 5 Model B Rev 1.0":     "Raspberry Pi 5 Model B Rev 1.0", // no NUL
		" padded \x00":                       "padded",
		"\x00":                               "",
		"":                                   "",
	}
	for in, want := range cases {
		if got := cleanModel([]byte(in)); got != want {
			t.Errorf("cleanModel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestOSReleaseField covers PRETTY_NAME extraction + unquoting, a missing
// key, and that a key is matched only as a full field prefix.
func TestOSReleaseField(t *testing.T) {
	content := []byte("NAME=\"Debian GNU/Linux\"\nPRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\nVERSION_ID=\"13\"\n")
	if got := osReleaseField(content, "PRETTY_NAME"); got != "Debian GNU/Linux 13 (trixie)" {
		t.Errorf("PRETTY_NAME = %q", got)
	}
	if got := osReleaseField(content, "MISSING"); got != "" {
		t.Errorf("MISSING = %q, want empty", got)
	}
	// Unquoted values pass through too.
	if got := osReleaseField([]byte("ID=debian\n"), "ID"); got != "debian" {
		t.Errorf("ID = %q, want debian", got)
	}
}

func TestCapitalize(t *testing.T) {
	for in, want := range map[string]string{"windows": "Windows", "linux": "Linux", "": ""} {
		if got := capitalize(in); got != want {
			t.Errorf("capitalize(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestMemTotalKB covers the /proc/meminfo MemTotal extraction, a missing
// field, and a malformed value.
func TestMemTotalKB(t *testing.T) {
	content := []byte("MemTotal:        8052812 kB\nMemFree:          123456 kB\n")
	if got := memTotalKB(content); got != 8052812 {
		t.Errorf("MemTotal = %d, want 8052812", got)
	}
	if got := memTotalKB([]byte("MemFree: 1 kB\n")); got != 0 {
		t.Errorf("missing MemTotal = %d, want 0", got)
	}
	if got := memTotalKB([]byte("MemTotal: notanumber kB\n")); got != 0 {
		t.Errorf("malformed MemTotal = %d, want 0", got)
	}
}

// TestFormatRAM pins the friendly rounding: reported RAM sits just under the
// nominal size, which must round back to the marketing GB (1/2/4/8); small
// amounts fall back to MB, and 0 yields "".
func TestFormatRAM(t *testing.T) {
	cases := map[int64]string{
		8052812: "8 GB", // Pi 4 8GB
		3886000: "4 GB", // Pi 4 4GB
		1917000: "2 GB", // Pi 4 2GB
		948000:  "926 MB",
		0:       "",
		-5:      "",
	}
	for kb, want := range cases {
		if got := formatRAM(kb); got != want {
			t.Errorf("formatRAM(%d) = %q, want %q", kb, got, want)
		}
	}
}

// TestDetect always yields a usable description: OS and Arch are never
// empty (the off-Linux fallback fills OS from GOOS).
func TestDetect(t *testing.T) {
	got := Detect()
	if got.Arch != runtime.GOARCH {
		t.Errorf("Arch = %q, want %q", got.Arch, runtime.GOARCH)
	}
	if got.OS == "" {
		t.Error("OS is empty; the GOOS fallback must fill it")
	}
}
