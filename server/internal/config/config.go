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
	// Example: "http://127.0.0.1:8555/api/stream.mjpeg?src=front-door"
	StreamBackendURL string

	// --- Side-channel (Saison 17, cloud tier) ---
	//
	// All edge-side fields are optional: the cloud link is ADDITIVE,
	// so an edge with no side-channel config simply does not dial out
	// and runs fully locally. The cloud role, by contrast, needs the
	// listener plus its server mTLS material (see ValidateCloud).
	// These are CARVILON_-only (born in Saison 17); no UNIFIX_ alias.

	// SidechannelListenAddr is the cloud-role bind address.
	// Default ":8443".
	SidechannelListenAddr string
	// SidechannelDialURL is the edge-role cloud endpoint, e.g.
	// "wss://<vps-ip>:8443/sidechannel". The host must match the
	// server cert's IP SAN.
	SidechannelDialURL string
	// SidechannelCACert is the CA cert path (both roles).
	SidechannelCACert string
	// SidechannelServerCert / SidechannelServerKey are the cloud
	// server's own cert+key.
	SidechannelServerCert string
	SidechannelServerKey  string
	// SidechannelClientCert / SidechannelClientKey are the edge's own
	// cert+key, presented for mTLS.
	SidechannelClientCert string
	SidechannelClientKey  string
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
	// Side-channel (Saison 17). CARVILON_-only, no legacy alias.
	envSidechannelListenAddr = "CARVILON_SIDECHANNEL_LISTEN_ADDR"
	envSidechannelDialURL    = "CARVILON_SIDECHANNEL_DIAL_URL"
	envSidechannelCACert     = "CARVILON_SIDECHANNEL_CA_CERT"
	envSidechannelServerCert = "CARVILON_SIDECHANNEL_SERVER_CERT"
	envSidechannelServerKey  = "CARVILON_SIDECHANNEL_SERVER_KEY"
	envSidechannelClientCert = "CARVILON_SIDECHANNEL_CLIENT_CERT"
	envSidechannelClientKey  = "CARVILON_SIDECHANNEL_CLIENT_KEY"
	defaultSidechannelListen = ":8443"
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

		SidechannelListenAddr: lookupEnv(envSidechannelListenAddr),
		SidechannelDialURL:    lookupEnv(envSidechannelDialURL),
		SidechannelCACert:     lookupEnv(envSidechannelCACert),
		SidechannelServerCert: lookupEnv(envSidechannelServerCert),
		SidechannelServerKey:  lookupEnv(envSidechannelServerKey),
		SidechannelClientCert: lookupEnv(envSidechannelClientCert),
		SidechannelClientKey:  lookupEnv(envSidechannelClientKey),
	}
	if cfg.SidechannelListenAddr == "" {
		cfg.SidechannelListenAddr = defaultSidechannelListen
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

// ValidateCloud checks the fields the cloud role needs: the
// side-channel listener plus its mTLS material. The cloud role runs
// none of the edge subsystems (no DB, no HTTP TLS cert, no mocks), so
// the edge-only fields are intentionally not validated here.
func (c Config) ValidateCloud() error {
	if c.SidechannelListenAddr == "" {
		return fmt.Errorf("config: %s must not be empty for the cloud role", envSidechannelListenAddr)
	}
	if c.SidechannelCACert == "" {
		return fmt.Errorf("config: %s is required for the cloud role", envSidechannelCACert)
	}
	if c.SidechannelServerCert == "" {
		return fmt.Errorf("config: %s is required for the cloud role", envSidechannelServerCert)
	}
	if c.SidechannelServerKey == "" {
		return fmt.Errorf("config: %s is required for the cloud role", envSidechannelServerKey)
	}
	return nil
}

// SidechannelClientConfigured reports whether the edge has enough
// config to dial the cloud. The link is additive: an edge missing any
// of these simply skips the client and runs fully locally, so this is
// a soft check (a skip-or-start decision), never a Validate() failure.
func (c Config) SidechannelClientConfigured() bool {
	return c.SidechannelDialURL != "" &&
		c.SidechannelCACert != "" &&
		c.SidechannelClientCert != "" &&
		c.SidechannelClientKey != ""
}

func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
