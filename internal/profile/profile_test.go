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
