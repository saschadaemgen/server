// Package config loads server runtime configuration from
// environment variables and validates it. Carvilon-server is a
// single-binary daemon, so config lives in the process
// environment rather than in a file: easier to inject via systemd
// unit files and trivial to override in dev.
package config

import (
	"encoding/hex"
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

	// SidechannelCloudWhipURL (edge) is the static cloud WHIP ingress
	// the stream-edge pushes to. Passed to the StreamPublisher, NOT
	// carried per frame. Optional (empty until the stream layer docks).
	SidechannelCloudWhipURL string
	// SidechannelInternalAddr (cloud) enables the interim localhost
	// request-publish HTTP hook when set (e.g. "127.0.0.1:8444").
	// Empty disables it. Interim until the stream-cloud layer triggers
	// publishes directly.
	SidechannelInternalAddr string

	// PublishTokenHMACKey (hex, 32 bytes / 64 chars) is the symmetric
	// key carvilon signs publish tokens with. It is its OWN env var,
	// not derived from the master key, because the stream-cloud layer
	// must hold the same key to verify - and the master key stays
	// isolated on the RPi. Required on the edge once
	// CARVILON_SIDECHANNEL_DIAL_URL is set (see Validate).
	PublishTokenHMACKey string

	// --- FCM doorbell push (Saison 17, edge role) ---
	//
	// Both optional and a pair: set together to enable FCM, leave both
	// empty to disable it (the edge starts normally and the push leg
	// skips). Setting exactly one is a config error (see Validate).
	// FCM runs on the edge (the RPi calls Google directly), not via the
	// cloud / side-channel.

	// FCMServiceAccountJSON is the path to the Firebase service-account
	// JSON used to mint the FCM access token.
	FCMServiceAccountJSON string
	// FCMProjectID is the Firebase project id used in the FCM v1 send
	// URL (projects/<id>/messages:send).
	FCMProjectID string
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
	envSidechannelClientCert   = "CARVILON_SIDECHANNEL_CLIENT_CERT"
	envSidechannelClientKey    = "CARVILON_SIDECHANNEL_CLIENT_KEY"
	envSidechannelCloudWhipURL = "CARVILON_SIDECHANNEL_CLOUD_WHIP_URL"
	envSidechannelInternalAddr = "CARVILON_SIDECHANNEL_INTERNAL_ADDR"
	envPublishTokenHMACKey     = "CARVILON_PUBLISH_TOKEN_HMAC_KEY"
	envFCMServiceAccountJSON   = "CARVILON_FCM_SERVICE_ACCOUNT_JSON"
	envFCMProjectID            = "CARVILON_FCM_PROJECT_ID"
	defaultSidechannelListen   = ":8443"
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

		SidechannelCloudWhipURL: lookupEnv(envSidechannelCloudWhipURL),
		SidechannelInternalAddr: lookupEnv(envSidechannelInternalAddr),

		PublishTokenHMACKey: lookupEnv(envPublishTokenHMACKey),

		FCMServiceAccountJSON: lookupEnv(envFCMServiceAccountJSON),
		FCMProjectID:          lookupEnv(envFCMProjectID),
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
	// Edge publish-token signing key: required once the side-channel is
	// being dialed (DIAL_URL set), because the EdgePublisher then issues
	// publish tokens. Optional otherwise (a pure-LAN edge needs none).
	if c.SidechannelDialURL != "" {
		if c.PublishTokenHMACKey == "" {
			return fmt.Errorf("config: %s is required when %s is set (edge publish-token signing key)",
				envPublishTokenHMACKey, envSidechannelDialURL)
		}
		if _, err := c.DecodePublishTokenHMACKey(); err != nil {
			return fmt.Errorf("config: %s invalid: %w", envPublishTokenHMACKey, err)
		}
	}
	// FCM is both-or-neither: either both the service-account path and
	// the project id are set (FCM enabled) or both empty (FCM disabled).
	// Exactly one set is a half-configuration, i.e. a config error.
	if (c.FCMServiceAccountJSON == "") != (c.FCMProjectID == "") {
		return fmt.Errorf("config: %s and %s must be set together (or both empty to disable FCM)",
			envFCMServiceAccountJSON, envFCMProjectID)
	}
	return nil
}

// FCMEnabled reports whether FCM doorbell push is configured (both the
// service-account path and the project id are present). When false the
// edge runs normally and the doorbell push leg skips.
func (c Config) FCMEnabled() bool {
	return c.FCMServiceAccountJSON != "" && c.FCMProjectID != ""
}

// DecodePublishTokenHMACKey hex-decodes the publish-token HMAC key and
// checks it is exactly 32 bytes (64 hex chars). Its own env var (not a
// master-key subkey) so the stream-cloud verifier can hold the same key
// while the master key stays isolated on the RPi.
func (c Config) DecodePublishTokenHMACKey() ([]byte, error) {
	b, err := hex.DecodeString(c.PublishTokenHMACKey)
	if err != nil {
		return nil, fmt.Errorf("must be hex: %w", err)
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("must be 32 bytes (64 hex chars), got %d", len(b))
	}
	return b, nil
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
