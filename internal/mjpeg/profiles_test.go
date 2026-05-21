package mjpeg

import (
	"strings"
	"testing"
)

func TestProfile_Validate_Defaults(t *testing.T) {
	for _, p := range DefaultProfiles() {
		if err := p.Validate(); err != nil {
			t.Errorf("default profile %q invalid: %v", p.Name, err)
		}
	}
}

func TestProfile_Validate_RejectsBadValues(t *testing.T) {
	good := Profile{Name: "x", Width: 100, Height: 100, FPS: 10, Quality: 5}
	cases := []struct {
		name   string
		mutate func(p *Profile)
		want   string
	}{
		{"empty Name", func(p *Profile) { p.Name = "" }, "Name"},
		{"zero Width", func(p *Profile) { p.Width = 0 }, "Width"},
		{"huge Width", func(p *Profile) { p.Width = 99999 }, "Width"},
		{"zero Height", func(p *Profile) { p.Height = 0 }, "Height"},
		{"zero FPS", func(p *Profile) { p.FPS = 0 }, "FPS"},
		{"huge FPS", func(p *Profile) { p.FPS = 1000 }, "FPS"},
		{"zero Quality", func(p *Profile) { p.Quality = 0 }, "Quality"},
		{"huge Quality", func(p *Profile) { p.Quality = 100 }, "Quality"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := good
			c.mutate(&p)
			err := p.Validate()
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q should mention %q", err.Error(), c.want)
			}
		})
	}
}

func TestProfile_OutputArgs_ESPDefault(t *testing.T) {
	// intercom_esp from DefaultProfiles: 800x1280 @ 9 fps q:v 6.
	// Verify the actual ffmpeg flags landed exactly where we expect.
	p := Profile{Name: "intercom_esp", Width: 800, Height: 1280, FPS: 9, Quality: 6}
	args := p.OutputArgs()

	want := []string{
		"-an",
		"-vf", "scale=800:1280",
		"-r", "9",
		"-c:v", "mjpeg",
		"-q:v", "6",
		"-f", "mjpeg",
	}
	if len(args) != len(want) {
		t.Fatalf("len(args) = %d, want %d (%v)", len(args), len(want), args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, args[i], want[i])
		}
	}
}

func TestProfile_OutputArgs_BrowserDefault(t *testing.T) {
	p := Profile{Name: "intercom_browser", Width: 640, Height: 1024, FPS: 12, Quality: 5}
	args := p.OutputArgs()
	// Spot-check the values we care about most.
	got := strings.Join(args, " ")
	for _, want := range []string{"scale=640:1024", "-r 12", "-q:v 5"} {
		if !strings.Contains(got, want) {
			t.Errorf("OutputArgs missing %q in %q", want, got)
		}
	}
}

func TestDefaultProfiles_StableNames(t *testing.T) {
	// The names are part of the drop-in contract with go2rtc / the
	// carvilon proxy. They must NOT change without a coordinated
	// migration.
	wantNames := map[string]bool{
		"intercom_esp":     true,
		"intercom_browser": true,
	}
	got := DefaultProfiles()
	if len(got) != len(wantNames) {
		t.Fatalf("DefaultProfiles count = %d, want %d", len(got), len(wantNames))
	}
	for _, p := range got {
		if !wantNames[p.Name] {
			t.Errorf("unexpected default profile name %q", p.Name)
		}
	}
}
