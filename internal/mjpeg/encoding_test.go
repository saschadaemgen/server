package mjpeg

import (
	"errors"
	"strings"
	"testing"

	"carvilon.local/stream/internal/profile"
)

func TestEncodeSpec_Validate_Good(t *testing.T) {
	s := EncodeSpec{Width: 640, Height: 1024, FPS: 12, Quality: 5}
	if err := s.Validate(); err != nil {
		t.Errorf("good spec failed validate: %v", err)
	}
}

func TestEncodeSpec_Validate_BadFields(t *testing.T) {
	good := EncodeSpec{Width: 640, Height: 1024, FPS: 12, Quality: 5}
	cases := []struct {
		name   string
		mutate func(*EncodeSpec)
		want   string
	}{
		{"zero Width", func(s *EncodeSpec) { s.Width = 0 }, "Width"},
		{"huge Width", func(s *EncodeSpec) { s.Width = 99999 }, "Width"},
		{"zero Height", func(s *EncodeSpec) { s.Height = 0 }, "Height"},
		{"zero FPS", func(s *EncodeSpec) { s.FPS = 0 }, "FPS"},
		{"huge FPS", func(s *EncodeSpec) { s.FPS = 1000 }, "FPS"},
		{"zero Quality", func(s *EncodeSpec) { s.Quality = 0 }, "Quality"},
		{"huge Quality", func(s *EncodeSpec) { s.Quality = 100 }, "Quality"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := good
			c.mutate(&s)
			err := s.Validate()
			if err == nil {
				t.Fatalf("expected error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q should mention %q", err.Error(), c.want)
			}
		})
	}
}

func TestEncodeSpec_OutputArgs_Order(t *testing.T) {
	// Lock down the exact ffmpeg argument layout. The order matters
	// because some flags apply only to the option immediately following
	// them (e.g. -vf, -r), and a regression here would silently corrupt
	// encoded output without a unit test signal.
	s := EncodeSpec{Width: 800, Height: 1280, FPS: 9, Quality: 6}
	args := s.OutputArgs()

	want := []string{
		"-an",
		"-vf", "scale=800:1280",
		"-r", "9",
		"-c:v", "mjpeg",
		"-q:v", "6",
		"-f", "mjpeg",
	}
	if len(args) != len(want) {
		t.Fatalf("len(args)=%d, want %d (got %v)", len(args), len(want), args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, args[i], want[i])
		}
	}
}

func TestDefaultSpecForUsage_Browser(t *testing.T) {
	spec, err := DefaultSpecForUsage(profile.UsageBrowser)
	if err != nil {
		t.Fatalf("DefaultSpecForUsage(browser): %v", err)
	}
	want := EncodeSpec{Width: 640, Height: 1024, FPS: 12, Quality: 5}
	if spec != want {
		t.Errorf("browser default = %+v, want %+v", spec, want)
	}
}

func TestDefaultSpecForUsage_ESP(t *testing.T) {
	spec, err := DefaultSpecForUsage(profile.UsageESP)
	if err != nil {
		t.Fatalf("DefaultSpecForUsage(esp): %v", err)
	}
	want := EncodeSpec{Width: 800, Height: 1280, FPS: 9, Quality: 6}
	if spec != want {
		t.Errorf("esp default = %+v, want %+v", spec, want)
	}
}

func TestDefaultSpecForUsage_UnknownReturnsErr(t *testing.T) {
	_, err := DefaultSpecForUsage(profile.Usage("android"))
	if !errors.Is(err, ErrUnknownUsage) {
		t.Errorf("err = %v, want ErrUnknownUsage chain", err)
	}
}
