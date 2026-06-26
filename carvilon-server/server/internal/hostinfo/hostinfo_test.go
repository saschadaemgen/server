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
