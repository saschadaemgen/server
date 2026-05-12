// Package config loads server runtime configuration from
// environment variables and validates it. Unifix-server is a
// single-binary daemon, so config lives in the process
// environment rather than in a file: easier to inject via systemd
// unit files and trivial to override in dev.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Config holds runtime settings for unifix-server.
type Config struct {
	// ListenAddr is the bind address. Default ":8443" for TLS,
	// ":8080" for DevMode.
	ListenAddr string

	// CertFile and KeyFile are PEM paths. Required unless DevMode.
	CertFile string
	KeyFile  string

	// DBPath is the SQLite database location. Default
	// "./state/unifix.db".
	DBPath string

	// DevMode enables plain HTTP and disables the Secure cookie
	// flag. Strictly for local development.
	DevMode bool

	// BaseURL is the externally visible URL of the server, used
	// for redirect targets and (later) magic-link emails.
	// Default in DevMode: "http://localhost:8080". In TLS mode
	// the operator must set it explicitly.
	BaseURL string

	// ServerIPv4 is the IPv4 address the embedded mock viewers
	// announce in discovery replies (TLV 0x02). Empty disables
	// mock viewers without preventing the server from starting.
	ServerIPv4 string

	// MockStateDir is the parent directory under which each
	// embedded mock viewer keeps its per-mock state and certs.
	// Default "./state/mocks".
	MockStateDir string

	// SecretsKeySet mirrors whether UNIFIX_SECRETS_KEY is set in
	// the environment. The actual key bytes are read by the
	// secrets package; Config only carries the boolean so
	// Validate can warn (not fail) when the operator forgot it.
	SecretsKeySet bool
}

const (
	defaultDBPath       = "./state/unifix.db"
	defaultListenDev    = ":8080"
	defaultListenTLS    = ":8443"
	defaultBaseURLDev   = "http://localhost:8080"
	defaultMockStateDir = "./state/mocks"
	envListenAddr       = "UNIFIX_LISTEN_ADDR"
	envCertFile         = "UNIFIX_CERT_FILE"
	envKeyFile          = "UNIFIX_KEY_FILE"
	envDBPath           = "UNIFIX_DB_PATH"
	envDevMode          = "UNIFIX_DEV_MODE"
	envBaseURL          = "UNIFIX_BASE_URL"
	envServerIPv4       = "UNIFIX_SERVER_IPV4"
	envMockStateDir     = "UNIFIX_MOCK_STATE_DIR"
	envSecretsKey       = "UNIFIX_SECRETS_KEY"
)

// FromEnv reads the unifix environment variables and fills in
// defaults for empty fields.
func FromEnv() Config {
	cfg := Config{
		ListenAddr:    os.Getenv(envListenAddr),
		CertFile:      os.Getenv(envCertFile),
		KeyFile:       os.Getenv(envKeyFile),
		DBPath:        os.Getenv(envDBPath),
		DevMode:       parseBool(os.Getenv(envDevMode)),
		BaseURL:       os.Getenv(envBaseURL),
		ServerIPv4:    os.Getenv(envServerIPv4),
		MockStateDir:  os.Getenv(envMockStateDir),
		SecretsKeySet: os.Getenv(envSecretsKey) != "",
	}
	if cfg.ListenAddr == "" {
		if cfg.DevMode {
			cfg.ListenAddr = defaultListenDev
		} else {
			cfg.ListenAddr = defaultListenTLS
		}
	}
	if cfg.DBPath == "" {
		cfg.DBPath = defaultDBPath
	}
	if cfg.BaseURL == "" && cfg.DevMode {
		cfg.BaseURL = defaultBaseURLDev
	}
	if cfg.MockStateDir == "" {
		cfg.MockStateDir = defaultMockStateDir
	}
	return cfg
}

// Validate checks that mandatory fields are present for the
// selected mode. TLS mode requires both CertFile and KeyFile;
// DevMode does not.
func (c Config) Validate() error {
	if c.ListenAddr == "" {
		return errors.New("config: ListenAddr must not be empty")
	}
	if c.DBPath == "" {
		return errors.New("config: DBPath must not be empty")
	}
	if !c.DevMode {
		if c.CertFile == "" {
			return fmt.Errorf("config: CertFile is required in TLS mode (set %s for plain HTTP)", envDevMode)
		}
		if c.KeyFile == "" {
			return fmt.Errorf("config: KeyFile is required in TLS mode (set %s for plain HTTP)", envDevMode)
		}
	}
	return nil
}

func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
