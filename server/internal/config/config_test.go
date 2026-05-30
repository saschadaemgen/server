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

func TestFromEnv_SidechannelListenDefault(t *testing.T) {
	t.Setenv(envSidechannelListenAddr, "")
	cfg := FromEnv()
	if cfg.SidechannelListenAddr != defaultSidechannelListen {
		t.Errorf("SidechannelListenAddr = %q, want %q", cfg.SidechannelListenAddr, defaultSidechannelListen)
	}
}

func TestValidateCloud_RequiresServerMaterial(t *testing.T) {
	full := Config{
		SidechannelListenAddr: ":8443",
		SidechannelCACert:     "ca.crt",
		SidechannelServerCert: "server.crt",
		SidechannelServerKey:  "server.key",
	}
	if err := full.ValidateCloud(); err != nil {
		t.Errorf("ValidateCloud(full) = %v, want nil", err)
	}
	for _, tc := range []struct {
		name string
		mut  func(*Config)
	}{
		{"no listen", func(c *Config) { c.SidechannelListenAddr = "" }},
		{"no ca", func(c *Config) { c.SidechannelCACert = "" }},
		{"no server cert", func(c *Config) { c.SidechannelServerCert = "" }},
		{"no server key", func(c *Config) { c.SidechannelServerKey = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := full
			tc.mut(&c)
			if err := c.ValidateCloud(); err == nil {
				t.Errorf("ValidateCloud(%s) = nil, want error", tc.name)
			}
		})
	}
}

// TestValidateCloud_IgnoresEdgeMaterial documents that the cloud role
// does not require the edge HTTP cert/key or DBPath: a config that
// carries only the side-channel server material is valid for cloud.
func TestValidateCloud_IgnoresEdgeMaterial(t *testing.T) {
	c := Config{
		SidechannelListenAddr: ":8443",
		SidechannelCACert:     "ca.crt",
		SidechannelServerCert: "server.crt",
		SidechannelServerKey:  "server.key",
		// deliberately no CertFile / KeyFile / DBPath
	}
	if err := c.ValidateCloud(); err != nil {
		t.Errorf("ValidateCloud without edge HTTP certs/DB = %v, want nil", err)
	}
}

func TestSidechannelClientConfigured(t *testing.T) {
	full := Config{
		SidechannelDialURL:    "wss://example:8443/sidechannel",
		SidechannelCACert:     "ca.crt",
		SidechannelClientCert: "client.crt",
		SidechannelClientKey:  "client.key",
	}
	if !full.SidechannelClientConfigured() {
		t.Error("full config: SidechannelClientConfigured() = false, want true")
	}
	for _, tc := range []struct {
		name string
		mut  func(*Config)
	}{
		{"no url", func(c *Config) { c.SidechannelDialURL = "" }},
		{"no ca", func(c *Config) { c.SidechannelCACert = "" }},
		{"no client cert", func(c *Config) { c.SidechannelClientCert = "" }},
		{"no client key", func(c *Config) { c.SidechannelClientKey = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := full
			tc.mut(&c)
			if c.SidechannelClientConfigured() {
				t.Errorf("%s: SidechannelClientConfigured() = true, want false", tc.name)
			}
		})
	}
}

func TestValidate_PublishTokenKey_RequiredWithSidechannel(t *testing.T) {
	validKey := strings.Repeat("ab", 32) // 64 hex chars = 32 bytes
	base := func() Config {
		return Config{ListenAddr: ":8080", DBPath: defaultDBPath, DevMode: true}
	}

	// No side-channel dial URL: the key is optional (pure-LAN edge).
	if err := base().Validate(); err != nil {
		t.Errorf("Validate (no dial url, no key) = %v, want nil", err)
	}

	// Dial URL set but no key: error.
	c := base()
	c.SidechannelDialURL = "wss://x:8443/sidechannel"
	if err := c.Validate(); err == nil {
		t.Error("Validate (dial url, no key) = nil, want error")
	}

	// Dial URL set, non-hex key: error.
	c.PublishTokenHMACKey = "nothex!!"
	if err := c.Validate(); err == nil {
		t.Error("Validate (dial url, non-hex key) = nil, want error")
	}

	// Dial URL set, wrong length (16 bytes): error.
	c.PublishTokenHMACKey = strings.Repeat("ab", 16)
	if err := c.Validate(); err == nil {
		t.Error("Validate (dial url, 16-byte key) = nil, want error")
	}

	// Dial URL set, valid 32-byte hex: ok.
	c.PublishTokenHMACKey = validKey
	if err := c.Validate(); err != nil {
		t.Errorf("Validate (dial url, valid key) = %v, want nil", err)
	}
}

func TestValidate_FCM_BothOrNeither(t *testing.T) {
	base := func() Config {
		return Config{ListenAddr: ":8080", DBPath: defaultDBPath, DevMode: true}
	}

	// Neither set: ok (FCM disabled).
	c := base()
	if err := c.Validate(); err != nil {
		t.Errorf("Validate (no FCM) = %v, want nil", err)
	}
	if c.FCMEnabled() {
		t.Error("FCMEnabled() = true with neither value set")
	}

	// Only the path set: error.
	c = base()
	c.FCMServiceAccountJSON = "/etc/carvilon/sa.json"
	if err := c.Validate(); err == nil {
		t.Error("Validate (only path) = nil, want error")
	}

	// Only the project id set: error.
	c = base()
	c.FCMProjectID = "my-project"
	if err := c.Validate(); err == nil {
		t.Error("Validate (only project id) = nil, want error")
	}

	// Both set: ok, enabled.
	c = base()
	c.FCMServiceAccountJSON = "/etc/carvilon/sa.json"
	c.FCMProjectID = "my-project"
	if err := c.Validate(); err != nil {
		t.Errorf("Validate (both FCM) = %v, want nil", err)
	}
	if !c.FCMEnabled() {
		t.Error("FCMEnabled() = false with both values set")
	}
}

func TestStreamInProcessConfigured(t *testing.T) {
	full := Config{
		StreamNVRHost:    "192.168.1.1",
		StreamAPIKey:     "secret-key",
		StreamDBPath:     "state/stream.db",
		StreamAddr:       ":8555",
		StreamBackendURL: "http://127.0.0.1:8555",
	}
	if !full.StreamInProcessConfigured() {
		t.Error("full config: StreamInProcessConfigured() = false, want true")
	}
	for _, tc := range []struct {
		name string
		mut  func(*Config)
	}{
		{"no nvr host", func(c *Config) { c.StreamNVRHost = "" }},
		{"no api key", func(c *Config) { c.StreamAPIKey = "" }},
		{"no db path", func(c *Config) { c.StreamDBPath = "" }},
		{"no addr", func(c *Config) { c.StreamAddr = "" }},
		{"no base url", func(c *Config) { c.StreamBackendURL = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := full
			tc.mut(&c)
			if c.StreamInProcessConfigured() {
				t.Errorf("%s: StreamInProcessConfigured() = true, want false", tc.name)
			}
		})
	}
}

func TestFromEnv_StreamInProcessFields(t *testing.T) {
	t.Setenv(envStreamNVRHost, "192.168.1.1")
	t.Setenv(envStreamAPIKey, "sekret")
	t.Setenv(envStreamDBPath, "/var/lib/stream.db")
	t.Setenv(envStreamEncryption, "srtp")
	t.Setenv(envStreamAddr, ":8555")
	t.Setenv(envStreamFFmpegPath, "/usr/bin/ffmpeg")
	t.Setenv(envStreamEnableMJPEG, "1")
	cfg := FromEnv()
	if cfg.StreamNVRHost != "192.168.1.1" || cfg.StreamAPIKey != "sekret" ||
		cfg.StreamDBPath != "/var/lib/stream.db" || cfg.StreamEncryption != "srtp" ||
		cfg.StreamAddr != ":8555" || cfg.StreamFFmpegPath != "/usr/bin/ffmpeg" ||
		!cfg.StreamEnableMJPEG {
		t.Errorf("stream in-process fields not read correctly: %+v", cfg)
	}
}

func TestDecodePublishTokenHMACKey(t *testing.T) {
	if _, err := (Config{PublishTokenHMACKey: strings.Repeat("ab", 32)}).DecodePublishTokenHMACKey(); err != nil {
		t.Errorf("valid 32-byte hex rejected: %v", err)
	}
	if _, err := (Config{PublishTokenHMACKey: "zz"}).DecodePublishTokenHMACKey(); err == nil {
		t.Error("non-hex key accepted")
	}
	if _, err := (Config{PublishTokenHMACKey: strings.Repeat("ab", 16)}).DecodePublishTokenHMACKey(); err == nil {
		t.Error("16-byte key accepted")
	}
}
