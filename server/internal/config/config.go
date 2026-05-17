// Package config loads server runtime configuration from
// environment variables and validates it. Carvilon-server is a
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

// Config holds runtime settings for carvilon-server.
type Config struct {
	// ListenAddr is the bind address. Default ":8443" for TLS,
	// ":8080" for DevMode.
	ListenAddr string

	// CertFile and KeyFile are PEM paths. Required unless DevMode.
	CertFile string
	KeyFile  string

	// DBPath is the SQLite database location. Default
	// "./state/carvilon.db".
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

	// SecretsKeySet mirrors whether CARVILON_SECRETS_KEY (or the
	// legacy UNIFIX_SECRETS_KEY) is set in the environment. The
	// actual key bytes are read by the secrets package; Config
	// only carries the boolean so Validate can warn (not fail)
	// when the operator forgot it.
	SecretsKeySet bool

	// StreamBackendURL is the upstream URL the /esp/stream.mjpeg
	// reverse-proxy forwards to (saison-13-08). Empty means the
	// endpoint returns 503 - useful while the go2rtc / Protect
	// integration is still being plumbed.
	// Example: "http://127.0.0.1:1984/api/stream.mjpeg?src=front-door"
	StreamBackendURL string
}

const (
	defaultDBPath       = "./state/carvilon.db"
	defaultListenDev    = ":8080"
	defaultListenTLS    = ":8443"
	defaultBaseURLDev   = "http://localhost:8080"
	defaultMockStateDir = "./state/mocks"
	// Canonical CARVILON_* env-var names. The matching
	// UNIFIX_* legacy aliases below stay accepted by lookupEnv()
	// so a dev-script still exporting the old names keeps working
	// through a Saison-14 transition cycle.
	envListenAddr       = "CARVILON_LISTEN_ADDR"
	envCertFile         = "CARVILON_CERT_FILE"
	envKeyFile          = "CARVILON_KEY_FILE"
	envDBPath           = "CARVILON_DB_PATH"
	envDevMode          = "CARVILON_DEV_MODE"
	envBaseURL          = "CARVILON_BASE_URL"
	envServerIPv4       = "CARVILON_SERVER_IPV4"
	envMockStateDir     = "CARVILON_MOCK_STATE_DIR"
	envSecretsKey       = "CARVILON_SECRETS_KEY"
	envStreamBackendURL = "CARVILON_STREAM_BACKEND_URL"
	// Legacy aliases (Saison 14 rename, deprecation horizon S18+).
	legacyListenAddr       = "UNIFIX_LISTEN_ADDR"
	legacyCertFile         = "UNIFIX_CERT_FILE"
	legacyKeyFile          = "UNIFIX_KEY_FILE"
	legacyDBPath           = "UNIFIX_DB_PATH"
	legacyDevMode          = "UNIFIX_DEV_MODE"
	legacyBaseURL          = "UNIFIX_BASE_URL"
	legacyServerIPv4       = "UNIFIX_SERVER_IPV4"
	legacyMockStateDir     = "UNIFIX_MOCK_STATE_DIR"
	legacySecretsKey       = "UNIFIX_SECRETS_KEY"
	legacyStreamBackendURL = "UNIFIX_STREAM_BACKEND_URL"
)

// lookupEnv returns the first non-empty env-var value from the
// given names. Carvilon-prefixed names always come first; the
// UNIFIX_* aliases stay accepted as a Saison-14 backwards-compat
// for dev workflows still exporting the old spelling.
func lookupEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// FromEnv reads the carvilon environment variables and fills in
// defaults for empty fields.
func FromEnv() Config {
	cfg := Config{
		ListenAddr:       lookupEnv(envListenAddr, legacyListenAddr),
		CertFile:         lookupEnv(envCertFile, legacyCertFile),
		KeyFile:          lookupEnv(envKeyFile, legacyKeyFile),
		DBPath:           lookupEnv(envDBPath, legacyDBPath),
		DevMode:          parseBool(lookupEnv(envDevMode, legacyDevMode)),
		BaseURL:          lookupEnv(envBaseURL, legacyBaseURL),
		ServerIPv4:       lookupEnv(envServerIPv4, legacyServerIPv4),
		MockStateDir:     lookupEnv(envMockStateDir, legacyMockStateDir),
		SecretsKeySet:    lookupEnv(envSecretsKey, legacySecretsKey) != "",
		StreamBackendURL: lookupEnv(envStreamBackendURL, legacyStreamBackendURL),
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
