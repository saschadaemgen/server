package config

import (
	"strings"
	"testing"
)

func TestFromEnv_DefaultsWhenEmpty(t *testing.T) {
	for _, k := range []string{
		envListenAddr, envCertFile, envKeyFile,
		envDBPath, envDevMode, envBaseURL,
		envServerIPv4, envMockStateDir,
	} {
		t.Setenv(k, "")
	}
	cfg := FromEnv()
	if cfg.ListenAddr != defaultListenTLS {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, defaultListenTLS)
	}
	if cfg.DBPath != defaultDBPath {
		t.Errorf("DBPath = %q, want %q", cfg.DBPath, defaultDBPath)
	}
	if cfg.DevMode {
		t.Errorf("DevMode = true, want false by default")
	}
	if cfg.BaseURL != "" {
		t.Errorf("BaseURL = %q, want empty in TLS mode", cfg.BaseURL)
	}
	if cfg.ServerIPv4 != "" {
		t.Errorf("ServerIPv4 = %q, want empty", cfg.ServerIPv4)
	}
	if cfg.MockStateDir != defaultMockStateDir {
		t.Errorf("MockStateDir = %q, want %q", cfg.MockStateDir, defaultMockStateDir)
	}
}

func TestFromEnv_DevModeDefaults(t *testing.T) {
	for _, k := range []string{
		envListenAddr, envCertFile, envKeyFile,
		envDBPath, envBaseURL,
	} {
		t.Setenv(k, "")
	}
	t.Setenv(envDevMode, "1")
	cfg := FromEnv()
	if !cfg.DevMode {
		t.Fatal("DevMode = false, want true")
	}
	if cfg.ListenAddr != defaultListenDev {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, defaultListenDev)
	}
	if cfg.BaseURL != defaultBaseURLDev {
		t.Errorf("BaseURL = %q, want %q", cfg.BaseURL, defaultBaseURLDev)
	}
}

func TestFromEnv_OverridesApplied(t *testing.T) {
	t.Setenv(envListenAddr, ":9000")
	t.Setenv(envCertFile, "/etc/cert.pem")
	t.Setenv(envKeyFile, "/etc/key.pem")
	t.Setenv(envDBPath, "/var/lib/carvilon.db")
	t.Setenv(envDevMode, "false")
	t.Setenv(envBaseURL, "https://example.com")
	t.Setenv(envServerIPv4, "192.168.1.42")
	t.Setenv(envMockStateDir, "/var/lib/carvilon/mocks")
	cfg := FromEnv()
	if cfg.ListenAddr != ":9000" {
		t.Errorf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.CertFile != "/etc/cert.pem" {
		t.Errorf("CertFile = %q", cfg.CertFile)
	}
	if cfg.KeyFile != "/etc/key.pem" {
		t.Errorf("KeyFile = %q", cfg.KeyFile)
	}
	if cfg.DBPath != "/var/lib/carvilon.db" {
		t.Errorf("DBPath = %q", cfg.DBPath)
	}
	if cfg.DevMode {
		t.Error("DevMode = true, want false")
	}
	if cfg.BaseURL != "https://example.com" {
		t.Errorf("BaseURL = %q", cfg.BaseURL)
	}
	if cfg.ServerIPv4 != "192.168.1.42" {
		t.Errorf("ServerIPv4 = %q", cfg.ServerIPv4)
	}
	if cfg.MockStateDir != "/var/lib/carvilon/mocks" {
		t.Errorf("MockStateDir = %q", cfg.MockStateDir)
	}
}

func TestFromEnv_DevModeBoolForms(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"yes", true},
		{"  yes  ", true},
		{"0", false},
		{"false", false},
		{"no", false},
		{"", false},
		{"foo", false},
	}
	for _, c := range cases {
		t.Run(c.val, func(t *testing.T) {
			t.Setenv(envDevMode, c.val)
			cfg := FromEnv()
			if cfg.DevMode != c.want {
				t.Errorf("DevMode for %q = %v, want %v", c.val, cfg.DevMode, c.want)
			}
		})
	}
}

func TestValidate_TLSMode_RequiresCerts(t *testing.T) {
	c := Config{ListenAddr: ":8443", DBPath: defaultDBPath, DevMode: false}
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate without certs in TLS mode returned nil")
	}
	if !strings.Contains(err.Error(), "CertFile") {
		t.Errorf("error %q does not mention CertFile", err)
	}

	c.CertFile = "cert.pem"
	if err := c.Validate(); err == nil {
		t.Fatal("Validate with CertFile but no KeyFile returned nil")
	}

	c.KeyFile = "key.pem"
	if err := c.Validate(); err != nil {
		t.Errorf("Validate with both certs in TLS mode = %v, want nil", err)
	}
}

func TestValidate_DevMode_NoCertsRequired(t *testing.T) {
	c := Config{
		ListenAddr: ":8080",
		DBPath:     defaultDBPath,
		DevMode:    true,
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate in DevMode without certs = %v, want nil", err)
	}
}

func TestValidate_EmptyListenAddr_Rejected(t *testing.T) {
	c := Config{DBPath: defaultDBPath, DevMode: true}
	if err := c.Validate(); err == nil {
		t.Fatal("Validate with empty ListenAddr returned nil")
	}
}

func TestValidate_EmptyDBPath_Rejected(t *testing.T) {
	c := Config{ListenAddr: ":8080", DevMode: true}
	if err := c.Validate(); err == nil {
		t.Fatal("Validate with empty DBPath returned nil")
	}
}
