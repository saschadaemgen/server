package mock

import (
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

func validConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		MAC:         "0c:ea:14:42:42:42",
		IPv4:        "192.168.1.42",
		Name:        "Test Viewer",
		ServicePort: 8080,
		StateDir:    filepath.Join(t.TempDir(), "state"),
	}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---------- Config ----------

func TestConfig_Validate_AcceptsValid(t *testing.T) {
	cfg := validConfig(t)
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate on valid config = %v, want nil", err)
	}
}

func TestConfig_Validate_RejectsMissingMAC(t *testing.T) {
	cfg := validConfig(t)
	cfg.MAC = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate with empty MAC returned nil")
	}
}

func TestConfig_Validate_RejectsMissingIPv4(t *testing.T) {
	cfg := validConfig(t)
	cfg.IPv4 = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate with empty IPv4 returned nil")
	}
}

func TestConfig_Validate_RejectsInvalidMACFormat(t *testing.T) {
	cfg := validConfig(t)
	cfg.MAC = "not-a-mac"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate with invalid MAC returned nil")
	}
	if !strings.Contains(err.Error(), "MAC") {
		t.Errorf("error %q does not mention MAC", err)
	}
}

func TestConfig_Validate_RejectsInvalidIPv4(t *testing.T) {
	cfg := validConfig(t)
	cfg.IPv4 = "999.999.999.999"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate with bogus IPv4 returned nil")
	}
}

func TestConfig_Validate_RejectsZeroPort(t *testing.T) {
	cfg := validConfig(t)
	cfg.ServicePort = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate with zero port returned nil")
	}
}

func TestConfig_Validate_RejectsEmptyStateDir(t *testing.T) {
	cfg := validConfig(t)
	cfg.StateDir = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate with empty StateDir returned nil")
	}
}

// ---------- New ----------

func TestNew_ReturnsViewerWithCorrectMAC(t *testing.T) {
	cfg := validConfig(t)
	v, err := New(cfg, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if v.MAC() != cfg.MAC {
		t.Errorf("MAC() = %q, want %q", v.MAC(), cfg.MAC)
	}
	if v.Identity().Name != cfg.Name {
		t.Errorf("Identity.Name = %q, want %q", v.Identity().Name, cfg.Name)
	}
}

func TestNew_DerivesNameFromMACWhenEmpty(t *testing.T) {
	cfg := validConfig(t)
	cfg.Name = ""
	v, err := New(cfg, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Default name uses last 4 hex chars of the MAC, lowercase.
	want := "UA Intercom Viewer 4242"
	if v.Identity().Name != want {
		t.Errorf("Identity.Name = %q, want %q", v.Identity().Name, want)
	}
}

func TestNew_NilLoggerUsesDefault(t *testing.T) {
	cfg := validConfig(t)
	if _, err := New(cfg, nil); err != nil {
		t.Errorf("New with nil logger = %v, want nil", err)
	}
}

func TestNew_PropagatesValidateError(t *testing.T) {
	cfg := validConfig(t)
	cfg.MAC = ""
	if _, err := New(cfg, quietLogger()); err == nil {
		t.Fatal("New with invalid config returned nil error")
	}
}

// ---------- Channels ----------

func TestViewer_Events_ChannelIsBufferedAndNonNil(t *testing.T) {
	v, err := New(validConfig(t), quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch := v.Events()
	if ch == nil {
		t.Fatal("Events() returned nil")
	}
	if cap(v.events) != eventBuffer {
		t.Errorf("events capacity = %d, want %d", cap(v.events), eventBuffer)
	}
}

func TestViewer_Cancels_ChannelIsBufferedAndNonNil(t *testing.T) {
	v, err := New(validConfig(t), quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ch := v.Cancels()
	if ch == nil {
		t.Fatal("Cancels() returned nil")
	}
	if cap(v.cancels) != cancelBuffer {
		t.Errorf("cancels capacity = %d, want %d", cap(v.cancels), cancelBuffer)
	}
}

// ---------- GenerateJWT ----------

func TestGenerateJWT_ReturnsSignedToken(t *testing.T) {
	tok, err := GenerateJWT(validConfig(t))
	if err != nil {
		t.Fatalf("GenerateJWT: %v", err)
	}
	if strings.Count(tok, ".") != 2 {
		t.Errorf("token does not look like a JWT: %q", tok)
	}
}

func TestGenerateJWT_PropagatesValidateError(t *testing.T) {
	cfg := validConfig(t)
	cfg.MAC = ""
	if _, err := GenerateJWT(cfg); err == nil {
		t.Fatal("GenerateJWT with invalid config returned nil error")
	}
}

// Saison 13-04.5-B: RejectDoorbell returns ErrRejectNotReady when
// stage 6 has not wired the publisher yet (Viewer.New / pre-Run
// state). Live integration is covered by the mockmanager bridge
// tests on the server side.
func TestRejectDoorbell_NotReadyBeforeRun(t *testing.T) {
	v, err := New(validConfig(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := v.RejectDoorbell("28704e31e29c"); err != ErrRejectNotReady {
		t.Errorf("err = %v, want ErrRejectNotReady", err)
	}
}
