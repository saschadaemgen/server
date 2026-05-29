package main

import (
	"testing"

	"carvilon.local/stream/internal/profile"
)

// TestLoadSeedProfiles_BuiltInDefaultSet covers the S6-03 spike-only
// fallback: with neither CARVILON_PROFILES_JSON nor UNIFI_CAMERA_ID
// set, loadSeedProfiles returns the five-profile measurement set
// pointing at the built-in intercom CameraID. The briefing's success
// criterion ("go run .\cmd\spike on an empty DB seeds from the
// built-in default set").
func TestLoadSeedProfiles_BuiltInDefaultSet(t *testing.T) {
	t.Setenv("CARVILON_PROFILES_JSON", "")
	t.Setenv("UNIFI_CAMERA_ID", "")

	ps, err := loadSeedProfiles()
	if err != nil {
		t.Fatalf("loadSeedProfiles: %v", err)
	}
	wantNames := []string{"intercom_web", "mjpeg_hq", "mjpeg_bal", "mjpeg_fast", "h264_cbp"}
	if len(ps) != len(wantNames) {
		t.Fatalf("got %d profiles, want %d (%v)", len(ps), len(wantNames), wantNames)
	}
	for i, name := range wantNames {
		if ps[i].Name != name {
			t.Errorf("ps[%d].Name = %q, want %q", i, ps[i].Name, name)
		}
		// Built-in default set MUST use the spike's built-in camera ID;
		// otherwise a user with no UNIFI_CAMERA_ID would seed garbage.
		if ps[i].CameraID != defaultIntercomCameraID {
			t.Errorf("ps[%d].CameraID = %q, want %q", i, ps[i].CameraID, defaultIntercomCameraID)
		}
		// Every profile must validate — this is what tripped the live
		// run before S6-03 (Codec was missing). Lock it in.
		if err := ps[i].Validate(); err != nil {
			t.Errorf("ps[%d] (%s) failed Validate: %v", i, name, err)
		}
	}
}

// TestLoadSeedProfiles_UNIFI_CAMERA_ID_OverridesCameraButKeepsSet:
// setting UNIFI_CAMERA_ID changes WHICH camera the default-set targets,
// but the set itself (names, codecs, encode params) stays the same —
// the operator is just running the measurement campaign on a different
// physical camera.
func TestLoadSeedProfiles_UNIFI_CAMERA_ID_OverridesCameraButKeepsSet(t *testing.T) {
	t.Setenv("CARVILON_PROFILES_JSON", "")
	t.Setenv("UNIFI_CAMERA_ID", "some-other-cam-id")

	ps, err := loadSeedProfiles()
	if err != nil {
		t.Fatalf("loadSeedProfiles: %v", err)
	}
	if len(ps) != 5 {
		t.Fatalf("got %d profiles, want 5", len(ps))
	}
	for _, p := range ps {
		if p.CameraID != "some-other-cam-id" {
			t.Errorf("profile %q CameraID = %q, want some-other-cam-id", p.Name, p.CameraID)
		}
	}
}

// TestLoadSeedProfiles_JSON_OverridesEverything: CARVILON_PROFILES_JSON
// wins over both UNIFI_CAMERA_ID and the built-in default-set. The
// admin explicitly spelled out what they want, we obey.
func TestLoadSeedProfiles_JSON_OverridesEverything(t *testing.T) {
	t.Setenv("UNIFI_CAMERA_ID", "ignored")
	t.Setenv("CARVILON_PROFILES_JSON", `[
		{"name":"only_one","cameraID":"explicit","quality":"high","usage":"browser","codec":"h264_passthrough"}
	]`)

	ps, err := loadSeedProfiles()
	if err != nil {
		t.Fatalf("loadSeedProfiles: %v", err)
	}
	if len(ps) != 1 || ps[0].Name != "only_one" {
		t.Fatalf("JSON did not win over built-in / env defaults: %+v", ps)
	}
}

// TestLoadSeedProfiles_JSON_MalformedReturnsError keeps the existing
// JSON error path intact.
func TestLoadSeedProfiles_JSON_MalformedReturnsError(t *testing.T) {
	t.Setenv("CARVILON_PROFILES_JSON", `not-json`)
	_, err := loadSeedProfiles()
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

// TestDefaultMeasurementProfileSet_BriefingParameters locks in the
// briefing-pinned numbers so a refactor can't quietly drift them.
// Adjust BOTH here and in defaultMeasurementProfileSet if a future
// briefing tunes the defaults — the test failure is the reminder.
func TestDefaultMeasurementProfileSet_BriefingParameters(t *testing.T) {
	const cam = "test-cam"
	ps := defaultMeasurementProfileSet(cam)

	want := []profile.Profile{
		{Name: "intercom_web", Usage: profile.UsageBrowser, Codec: profile.CodecH264Passthrough},
		{Name: "mjpeg_hq", Usage: profile.UsageESP, Codec: profile.CodecMJPEG, Width: 800, Height: 1280, FPS: 10, EncodeQuality: 4},
		{Name: "mjpeg_bal", Usage: profile.UsageESP, Codec: profile.CodecMJPEG, Width: 800, Height: 1280, FPS: 12, EncodeQuality: 6},
		{Name: "mjpeg_fast", Usage: profile.UsageESP, Codec: profile.CodecMJPEG, Width: 640, Height: 1024, FPS: 18, EncodeQuality: 6},
		{Name: "h264_cbp", Usage: profile.UsageESP, Codec: profile.CodecH264CBP, Width: 800, Height: 1280, FPS: 15, EncodeQuality: 26},
	}
	if len(ps) != len(want) {
		t.Fatalf("got %d profiles, want %d", len(ps), len(want))
	}
	for i, w := range want {
		got := ps[i]
		if got.Name != w.Name {
			t.Errorf("ps[%d].Name = %q, want %q", i, got.Name, w.Name)
		}
		if got.Usage != w.Usage {
			t.Errorf("%s.Usage = %q, want %q", w.Name, got.Usage, w.Usage)
		}
		if got.Codec != w.Codec {
			t.Errorf("%s.Codec = %q, want %q", w.Name, got.Codec, w.Codec)
		}
		// Passthrough ignores encode params — only check them for
		// transcoded codecs.
		if w.Codec != profile.CodecH264Passthrough {
			if got.Width != w.Width || got.Height != w.Height || got.FPS != w.FPS || got.EncodeQuality != w.EncodeQuality {
				t.Errorf("%s encode params = (%d,%d,%d,%d), want (%d,%d,%d,%d)",
					w.Name, got.Width, got.Height, got.FPS, got.EncodeQuality,
					w.Width, w.Height, w.FPS, w.EncodeQuality)
			}
		}
	}
}

// TestSeedSource_NoEnv reflects the new "built-in" fallback label so
// the live log line on a zero-config start is actionable ("the spike
// is using its built-in set, not your env"). Locks the cosmetic
// message in case a refactor "tidies" it away.
func TestSeedSource_NoEnv(t *testing.T) {
	t.Setenv("CARVILON_PROFILES_JSON", "")
	t.Setenv("UNIFI_CAMERA_ID", "")
	got := seedSource()
	if got == "" {
		t.Fatal("seedSource returned empty string")
	}
	// Must mention "built-in" so log greppers can find it.
	if !contains(got, "built-in") {
		t.Errorf("seedSource = %q; want a label that mentions 'built-in'", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
