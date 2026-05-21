package profile

import (
	"errors"
	"strings"
	"testing"
)

var goodProfile = Profile{
	Name:        "intercom_browser",
	CameraID:    "679573e101080b03e4000424",
	Quality:     QualityHigh,
	Usage:       UsageBrowser,
	Description: "Intercom (browser)",
	// S6-01: h264_passthrough doesn't require encode params (the camera
	// dictates the wire shape); the field still has to be set explicitly,
	// the empty string is rejected.
	Codec: CodecH264Passthrough,
}

// goodMJPEGProfile is the canonical S6-01 transcoded-codec fixture used
// in tests that need a profile with encode parameters set.
var goodMJPEGProfile = Profile{
	Name:          "intercom_esp_mjpeg",
	CameraID:      "679573e101080b03e4000424",
	Quality:       QualityHigh,
	Usage:         UsageESP,
	Description:   "Intercom (ESP, MJPEG)",
	Codec:         CodecMJPEG,
	Width:         800,
	Height:        1280,
	FPS:           12,
	EncodeQuality: 6,
}

func TestProfile_Validate_Good(t *testing.T) {
	if err := goodProfile.Validate(); err != nil {
		t.Fatalf("good profile failed validate: %v", err)
	}
}

func TestProfile_Validate_BadFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Profile)
		want   string
	}{
		{"empty Name", func(p *Profile) { p.Name = "" }, "Name"},
		{"empty CameraID", func(p *Profile) { p.CameraID = "" }, "CameraID"},
		{"empty Quality", func(p *Profile) { p.Quality = "" }, "Quality"},
		{"bad Quality", func(p *Profile) { p.Quality = "ultra" }, "Quality"},
		{"empty Usage", func(p *Profile) { p.Usage = "" }, "Usage"},
		{"bad Usage", func(p *Profile) { p.Usage = "fridge" }, "Usage"},
		{"empty Codec", func(p *Profile) { p.Codec = "" }, "Codec"},
		{"bad Codec", func(p *Profile) { p.Codec = "av1" }, "Codec"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := goodProfile
			c.mutate(&p)
			err := p.Validate()
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q should mention %q", err.Error(), c.want)
			}
		})
	}
}

func TestRegistry_GetAndNames(t *testing.T) {
	p1 := goodProfile
	p2 := goodProfile
	p2.Name = "intercom_esp"
	p2.Usage = UsageESP

	r, err := NewRegistry([]Profile{p1, p2})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	got, err := r.Get("intercom_browser")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Usage != UsageBrowser {
		t.Errorf("got Usage=%q, want %q", got.Usage, UsageBrowser)
	}

	names := r.Names()
	if len(names) != 2 || names[0] != "intercom_browser" || names[1] != "intercom_esp" {
		t.Errorf("Names() = %v, want [intercom_browser intercom_esp]", names)
	}
}

func TestRegistry_GetUnknownReturnsErrUnknownProfile(t *testing.T) {
	r, _ := NewRegistry([]Profile{goodProfile})
	_, err := r.Get("does_not_exist")
	if !errors.Is(err, ErrUnknownProfile) {
		t.Errorf("err = %v, want ErrUnknownProfile chain", err)
	}
	if !strings.Contains(err.Error(), "does_not_exist") {
		t.Errorf("error %q should mention the missing name", err.Error())
	}
}

func TestRegistry_DuplicateNamesRejected(t *testing.T) {
	p := goodProfile
	_, err := NewRegistry([]Profile{p, p})
	if err == nil {
		t.Fatal("expected error for duplicate names")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error %q should mention 'duplicate'", err.Error())
	}
}

func TestRegistry_BadProfileRejectedAtStartup(t *testing.T) {
	bad := goodProfile
	bad.Quality = "bogus"
	_, err := NewRegistry([]Profile{bad})
	if err == nil {
		t.Fatal("expected error for invalid Quality")
	}
}

func TestRegistry_ByUsage(t *testing.T) {
	p1 := goodProfile
	p2 := goodProfile
	p2.Name = "intercom_esp"
	p2.Usage = UsageESP
	p3 := goodProfile
	p3.Name = "ai360_browser"
	p3.CameraID = "abc"

	r, _ := NewRegistry([]Profile{p1, p2, p3})

	browsers := r.ByUsage(UsageBrowser)
	if len(browsers) != 2 {
		t.Errorf("ByUsage(browser) returned %d, want 2", len(browsers))
	}
	for _, p := range browsers {
		if p.Usage != UsageBrowser {
			t.Errorf("ByUsage(browser) returned a non-browser profile %q", p.Name)
		}
	}

	esps := r.ByUsage(UsageESP)
	if len(esps) != 1 || esps[0].Name != "intercom_esp" {
		t.Errorf("ByUsage(esp) = %v, want [intercom_esp]", esps)
	}
}

func TestRegistry_AllSortedByName(t *testing.T) {
	p1 := goodProfile
	p1.Name = "zebra"
	p2 := goodProfile
	p2.Name = "alpha"
	p3 := goodProfile
	p3.Name = "mike"

	r, _ := NewRegistry([]Profile{p1, p2, p3})
	all := r.All()
	if len(all) != 3 {
		t.Fatalf("All() returned %d, want 3", len(all))
	}
	for i, want := range []string{"alpha", "mike", "zebra"} {
		if all[i].Name != want {
			t.Errorf("All()[%d].Name = %q, want %q", i, all[i].Name, want)
		}
	}
}

func TestRegistry_Put_InsertsAndOverwrites(t *testing.T) {
	r, _ := NewRegistry(nil)
	p := goodProfile

	// Insert
	if err := r.Put(p); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := r.Get(p.Name)
	if err != nil || got != p {
		t.Errorf("after first Put: got %+v err=%v, want %+v", got, err, p)
	}

	// Overwrite
	p2 := p
	p2.Description = "updated description"
	if err := r.Put(p2); err != nil {
		t.Fatalf("Put 2: %v", err)
	}
	got, _ = r.Get(p.Name)
	if got.Description != "updated description" {
		t.Errorf("upsert did not update Description: %+v", got)
	}
	if len(r.Names()) != 1 {
		t.Errorf("Names count = %d, want 1 after upsert", len(r.Names()))
	}
}

func TestRegistry_Put_RejectsInvalid(t *testing.T) {
	r, _ := NewRegistry(nil)
	bad := goodProfile
	bad.Quality = "bogus"
	if err := r.Put(bad); err == nil {
		t.Fatal("expected validation error")
	}
	// And the bad profile must NOT have been inserted.
	if len(r.Names()) != 0 {
		t.Errorf("invalid Put leaked into Registry: Names()=%v", r.Names())
	}
}

func TestRegistry_Delete_RemovesAndErrorsOnSecond(t *testing.T) {
	r, _ := NewRegistry([]Profile{goodProfile})

	if err := r.Delete(goodProfile.Name); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Get(goodProfile.Name); !errors.Is(err, ErrUnknownProfile) {
		t.Errorf("after Delete Get err = %v, want ErrUnknownProfile", err)
	}
	if err := r.Delete(goodProfile.Name); !errors.Is(err, ErrUnknownProfile) {
		t.Errorf("re-delete err = %v, want ErrUnknownProfile", err)
	}
}

func TestRegistry_EmptyIsOK(t *testing.T) {
	r, err := NewRegistry(nil)
	if err != nil {
		t.Fatalf("NewRegistry(nil): %v", err)
	}
	if len(r.Names()) != 0 {
		t.Errorf("empty registry should have no names, got %v", r.Names())
	}
	if _, err := r.Get("anything"); !errors.Is(err, ErrUnknownProfile) {
		t.Errorf("Get on empty: %v, want ErrUnknownProfile", err)
	}
}

// TestProfile_Validate_Passthrough_IgnoresEncodeParams asserts that the
// camera dictates wire shape for h264_passthrough: leaving Width/Height/
// FPS/EncodeQuality at zero is intentional and must NOT error.
func TestProfile_Validate_Passthrough_IgnoresEncodeParams(t *testing.T) {
	p := goodProfile // Codec = CodecH264Passthrough, zeroed encode params
	if p.Width != 0 || p.Height != 0 || p.FPS != 0 || p.EncodeQuality != 0 {
		t.Fatalf("test premise broken: goodProfile already has encode params: %+v", p)
	}
	if err := p.Validate(); err != nil {
		t.Errorf("h264_passthrough with zero encode params should validate; got %v", err)
	}
}

// TestProfile_Validate_MJPEG_Good is the analogue for the transcoded
// codec — encode params populated, validates clean.
func TestProfile_Validate_MJPEG_Good(t *testing.T) {
	if err := goodMJPEGProfile.Validate(); err != nil {
		t.Errorf("good mjpeg profile failed validate: %v", err)
	}
}

// TestProfile_Validate_TranscodedCodecs_RequireEncodeParams covers the
// other half of the codec/encode-param coupling: for mjpeg and h264_cbp
// the four encode fields are required and range-checked. The matrix
// exercises each field separately for both transcoded codecs.
func TestProfile_Validate_TranscodedCodecs_RequireEncodeParams(t *testing.T) {
	type mutateFn func(*Profile)
	codecs := []Codec{CodecMJPEG, CodecH264CBP}
	cases := []struct {
		name   string
		mutate mutateFn
		want   string
	}{
		{"Width=0", func(p *Profile) { p.Width = 0 }, "Width"},
		{"Width too big", func(p *Profile) { p.Width = 99999 }, "Width"},
		{"Height=0", func(p *Profile) { p.Height = 0 }, "Height"},
		{"Height too big", func(p *Profile) { p.Height = 99999 }, "Height"},
		{"FPS=0", func(p *Profile) { p.FPS = 0 }, "FPS"},
		{"FPS too big", func(p *Profile) { p.FPS = 200 }, "FPS"},
		{"EncodeQuality=0", func(p *Profile) { p.EncodeQuality = 0 }, "EncodeQuality"},
		{"EncodeQuality too big", func(p *Profile) { p.EncodeQuality = 99 }, "EncodeQuality"},
	}
	for _, codec := range codecs {
		for _, c := range cases {
			t.Run(string(codec)+"/"+c.name, func(t *testing.T) {
				p := goodMJPEGProfile
				p.Codec = codec
				c.mutate(&p)
				err := p.Validate()
				if err == nil {
					t.Fatalf("expected error for %s with %s", codec, c.name)
				}
				if !strings.Contains(err.Error(), c.want) {
					t.Errorf("error %q should mention %q", err.Error(), c.want)
				}
			})
		}
	}
}

// TestProfile_Validate_H264CBP_Good — symmetric coverage for the new
// constrained-baseline H.264 codec.
func TestProfile_Validate_H264CBP_Good(t *testing.T) {
	p := goodMJPEGProfile
	p.Name = "h264_cbp"
	p.Codec = CodecH264CBP
	p.EncodeQuality = 26 // CRF
	if err := p.Validate(); err != nil {
		t.Errorf("good h264_cbp profile failed validate: %v", err)
	}
}
