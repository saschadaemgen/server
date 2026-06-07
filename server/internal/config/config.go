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
	"net"
	"os"
	"strconv"
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

	// EgressTokenHMACKey (hex, 32 bytes / 64 chars) signs the short-lived
	// WHEP egress tokens (Saison 18-14). Its OWN env var, separate from
	// the publish key - that separate key IS the publish-vs-egress domain
	// separation. OPTIONAL: unset -> /webviewer/egress-token soft-503s
	// (the cloud egress is additive); if set it must be valid (Validate).
	EgressTokenHMACKey string

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

	// --- In-process stream server (Saison 17-08, carvilon_stream build) ---
	//
	// These feed stream.SetupEdgeInProcess in the carvilon_stream-tagged
	// wiring. They are READ in every build (plain env), but only CONSUMED
	// under the build tag; the public build ignores them. The typed
	// Encryption conversion happens in the tagged wiring (the profile type
	// is an internal stream package that the public build must not
	// import), so it stays a plain string here.
	//
	// BaseURL is intentionally NOT a separate value: the in-process server
	// reuses StreamBackendURL (CARVILON_STREAM_BACKEND_URL) as its MJPEG/
	// Offer base, because that env already means "the stream-server base
	// URL" and is already in the RPi env. StreamAddr is the listen address
	// of that same server.

	// StreamNVRHost is the UDM/Protect host (e.g. "192.168.1.1").
	StreamNVRHost string
	// StreamAPIKey is the Protect integration X-API-KEY. SECRET.
	StreamAPIKey string
	// StreamDBPath is the SQLite path for stream-profile persistence.
	StreamDBPath string
	// StreamEncryption is the camera-side wire mode ("tls"/"srtp"/"").
	// Empty -> tls (SetupEdgeInProcess maps it).
	StreamEncryption string
	// StreamAddr is the local stream-server HTTP listen address
	// (e.g. ":8555"). StreamBackendURL is the matching base URL.
	StreamAddr string
	// StreamFFmpegPath overrides the ffmpeg binary ("" -> "ffmpeg").
	StreamFFmpegPath string
	// StreamEnableMJPEG turns on the MJPEG / h264_cbp ffmpeg paths.
	StreamEnableMJPEG bool
	// StreamLANWHEPICEPort, when > 0, activates the edge LAN-direct WHEP
	// endpoint (Saison 19-35): the in-process stream server binds a
	// fixed-UDP-port ICE host candidate on this port, AND
	// /webviewer/stream-start advertises edge_whep_url so an on-LAN app
	// connects straight to the edge instead of looping through the VPS.
	// 0 (default) = off: no bind, no edge_whep_url. The HTTP /whep path
	// itself rides StreamAddr's port; this is the ICE (UDP) media port.
	StreamLANWHEPICEPort int

	// --- In-process cloud WHIP/WHEP stream (Saison 18-04, cloud role,
	// carvilon_stream build) ---
	//
	// The cloud mirror of the edge stream fields above. Read in every
	// build but only CONSUMED under the build tag (runCloud). Optional:
	// the cloud role can run side-channel-only. WhipCert + WhipKey set
	// together enable the in-process WHIP-ingress + WHEP-egress server
	// (CloudStreamInProcessConfigured); ValidateCloud then also requires
	// the publish-token HMAC key so the ingress can verify tokens.

	// WhipListen is the WHIP/WHEP TLS listen address (e.g. ":8444").
	// Empty -> the stream server defaults it to ":8444".
	WhipListen string
	// WhipCert / WhipKey are absolute paths to the WHIP server TLS
	// cert/key (PEM). Required together to enable the cloud stream.
	WhipCert string
	WhipKey  string

	// --- In-process TURN relay (Saison 18-05, cloud role,
	// carvilon_stream build) ---
	//
	// pion/turn embedded on the VPS so a CGNAT edge and a remote viewer
	// can find a media path. Read in every build, only CONSUMED under the
	// build tag. Optional and gated INDEPENDENTLY of WHIP
	// (CloudTURNConfigured): TURNPublicIP + TURNSharedSecret are the v1
	// minimum. Realm + ports default in FromEnv.

	// TURNPublicIP is the VPS's real public IP (the address the relay
	// advertises in ICE candidates). NOT a 172.x docker-bridge IP.
	TURNPublicIP string
	// TURNSharedSecret is the long-term secret the TURN server and the
	// credential minter share (RFC 5766 long-term credentials). SECRET:
	// never logged, never echoed into an error.
	TURNSharedSecret string
	// TURNRealm is the TURN realm. Empty -> "carvilon" (FromEnv default).
	TURNRealm string
	// TURNUDPPort / TURNTLSPort are the relay's UDP and TLS listen ports.
	// TURNUDPPort empty -> 3478 (the CGNAT workhorse). TURNTLSPort is
	// OPT-IN: empty/0 -> the turns: TLS relay is OFF; set
	// CARVILON_TURN_TLS_PORT (e.g. 5349) to enable the TLS leg for
	// networks that only allow outbound TLS.
	TURNUDPPort int
	TURNTLSPort int
	// TURNPublicHost is the public HOSTNAME the turns: leg is advertised
	// on (Saison 18-08). Empty -> no turns: line (stun + turn stay on the
	// IP). Only used when TURNTLSPort > 0.
	TURNPublicHost string
	// TURNTLSCertFile / TURNTLSKeyFile are the cert/key for the turns: TLS
	// leg, SEPARATE from the WHIP certs (e.g. a Let's Encrypt cert for
	// TURNPublicHost). Empty -> the stream falls back to the WHIP
	// CertFile/KeyFile (dev / single-cert setups). The WHIP ingress always
	// stays on the private cloudca.
	TURNTLSCertFile string
	TURNTLSKeyFile  string

	// --- Public WHEP egress (Saison 19-07 Baustufe 2, cloud role,
	// carvilon_stream build) ---
	//
	// A SEPARATE public WHEP-egress listener with a publicly-trusted cert, so
	// a remote browser/Android can subscribe over a browser-trusted endpoint.
	// The :8444 WHIP/WHEP listener (private cloudca) is UNTOUCHED - it stays
	// the edge publisher path. Opt-in via WHEPPublicAddr; the other three are
	// required once the addr is set (ValidateCloud). The cloud announces the
	// resulting public base to the edge over the side-channel, so the edge
	// needs no WHEP config of its own.

	// WHEPPublicAddr is the public WHEP-egress TLS listen address (e.g.
	// ":8446"). Empty -> the public WHEP listener is OFF.
	WHEPPublicAddr string
	// WHEPPublicHost is the public HOSTNAME the WHEP egress is advertised on
	// (e.g. the public WHEP host), matching the public cert's SAN. Builds the
	// base URL the cloud sends the edge. Required when WHEPPublicAddr is set.
	WHEPPublicHost string
	// WHEPPublicCert / WHEPPublicKey are the publicly-trusted cert/key (e.g.
	// Let's Encrypt for WHEPPublicHost), SEPARATE from the WHIP cloudca certs.
	// Required when WHEPPublicAddr is set.
	WHEPPublicCert string
	WHEPPublicKey  string

	// --- Cloud control endpoint (Saison 19-11 Baustufe 3, cloud role) ---
	//
	// A public HTTPS listener that lets a remote (Android) subscriber fetch
	// the stream-start bundle that today only the LAN edge serves. The cloud
	// relays the viewer Bearer to the edge over the side-channel
	// (bundle_request/bundle_reply), then assembles the bundle itself. Opt-in
	// via SignalPublicAddr; HOST + CERT + KEY are required once the addr is set
	// (ValidateCloud). Pure carvilon - the stream module is not involved.

	// SignalPublicAddr is the control-endpoint TLS listen address (e.g.
	// ":8447"). Empty -> the control endpoint is OFF.
	SignalPublicAddr string
	// SignalPublicHost is the public hostname the control endpoint is reached
	// on; used for logs / self-checks and matches the cert SAN. Required when
	// SignalPublicAddr is set.
	SignalPublicHost string
	// SignalPublicCert / SignalPublicKey are the publicly-trusted cert/key for
	// the control endpoint (its own Let's Encrypt cert, SEPARATE from the WHEP
	// and cloudca certs). Required when SignalPublicAddr is set.
	SignalPublicCert string
	SignalPublicKey  string
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
	envSidechannelListenAddr   = "CARVILON_SIDECHANNEL_LISTEN_ADDR"
	envSidechannelDialURL      = "CARVILON_SIDECHANNEL_DIAL_URL"
	envSidechannelCACert       = "CARVILON_SIDECHANNEL_CA_CERT"
	envSidechannelServerCert   = "CARVILON_SIDECHANNEL_SERVER_CERT"
	envSidechannelServerKey    = "CARVILON_SIDECHANNEL_SERVER_KEY"
	envSidechannelClientCert   = "CARVILON_SIDECHANNEL_CLIENT_CERT"
	envSidechannelClientKey    = "CARVILON_SIDECHANNEL_CLIENT_KEY"
	envSidechannelCloudWhipURL = "CARVILON_SIDECHANNEL_CLOUD_WHIP_URL"
	envSidechannelInternalAddr = "CARVILON_SIDECHANNEL_INTERNAL_ADDR"
	envPublishTokenHMACKey     = "CARVILON_PUBLISH_TOKEN_HMAC_KEY"
	envEgressTokenHMACKey      = "CARVILON_EGRESS_TOKEN_HMAC_KEY"
	envFCMServiceAccountJSON   = "CARVILON_FCM_SERVICE_ACCOUNT_JSON"
	envFCMProjectID            = "CARVILON_FCM_PROJECT_ID"
	envStreamNVRHost           = "CARVILON_STREAM_NVR_HOST"
	envStreamAPIKey            = "CARVILON_STREAM_API_KEY"
	envStreamDBPath            = "CARVILON_STREAM_DB_PATH"
	envStreamEncryption        = "CARVILON_STREAM_ENCRYPTION"
	envStreamAddr              = "CARVILON_STREAM_ADDR"
	envStreamFFmpegPath        = "CARVILON_STREAM_FFMPEG_PATH"
	envStreamEnableMJPEG       = "CARVILON_STREAM_ENABLE_MJPEG"
	envStreamLANWHEPICEPort    = "CARVILON_STREAM_LAN_WHEP_ICE_PORT"
	envWhipListen              = "CARVILON_WHIP_LISTEN"
	envWhipCert                = "CARVILON_WHIP_CERT"
	envWhipKey                 = "CARVILON_WHIP_KEY"
	envTURNPublicIP            = "CARVILON_TURN_PUBLIC_IP"
	envTURNSharedSecret        = "CARVILON_TURN_SHARED_SECRET"
	envTURNRealm               = "CARVILON_TURN_REALM"
	envTURNUDPPort             = "CARVILON_TURN_UDP_PORT"
	envTURNTLSPort             = "CARVILON_TURN_TLS_PORT"
	envTURNPublicHost          = "CARVILON_TURN_PUBLIC_HOST"
	envTURNTLSCert             = "CARVILON_TURN_TLS_CERT"
	envTURNTLSKey              = "CARVILON_TURN_TLS_KEY"
	envWHEPPublicAddr          = "CARVILON_WHEP_PUBLIC_ADDR"
	envWHEPPublicHost          = "CARVILON_WHEP_PUBLIC_HOST"
	envWHEPPublicCert          = "CARVILON_WHEP_PUBLIC_CERT"
	envWHEPPublicKey           = "CARVILON_WHEP_PUBLIC_KEY"
	envSignalPublicAddr        = "CARVILON_SIGNAL_PUBLIC_ADDR"
	envSignalPublicHost        = "CARVILON_SIGNAL_PUBLIC_HOST"
	envSignalPublicCert        = "CARVILON_SIGNAL_PUBLIC_CERT"
	envSignalPublicKey         = "CARVILON_SIGNAL_PUBLIC_KEY"
	defaultSidechannelListen   = ":8443"
	defaultTURNRealm           = "carvilon"
	defaultTURNUDPPort         = 3478
	defaultTURNTLSPort         = 0 // TLS relay is opt-in; 0 = off
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
		EgressTokenHMACKey:  lookupEnv(envEgressTokenHMACKey),

		FCMServiceAccountJSON: lookupEnv(envFCMServiceAccountJSON),
		FCMProjectID:          lookupEnv(envFCMProjectID),

		StreamNVRHost:        lookupEnv(envStreamNVRHost),
		StreamAPIKey:         lookupEnv(envStreamAPIKey),
		StreamDBPath:         lookupEnv(envStreamDBPath),
		StreamEncryption:     lookupEnv(envStreamEncryption),
		StreamAddr:           lookupEnv(envStreamAddr),
		StreamFFmpegPath:     lookupEnv(envStreamFFmpegPath),
		StreamEnableMJPEG:    parseBool(lookupEnv(envStreamEnableMJPEG)),
		StreamLANWHEPICEPort: parsePort(lookupEnv(envStreamLANWHEPICEPort), 0),

		WhipListen: lookupEnv(envWhipListen),
		WhipCert:   lookupEnv(envWhipCert),
		WhipKey:    lookupEnv(envWhipKey),

		TURNPublicIP:     lookupEnv(envTURNPublicIP),
		TURNSharedSecret: lookupEnv(envTURNSharedSecret),
		TURNRealm:        lookupEnv(envTURNRealm),
		TURNUDPPort:      parsePort(lookupEnv(envTURNUDPPort), defaultTURNUDPPort),
		TURNTLSPort:      parsePort(lookupEnv(envTURNTLSPort), defaultTURNTLSPort),
		TURNPublicHost:   lookupEnv(envTURNPublicHost),
		TURNTLSCertFile:  lookupEnv(envTURNTLSCert),
		TURNTLSKeyFile:   lookupEnv(envTURNTLSKey),

		WHEPPublicAddr: lookupEnv(envWHEPPublicAddr),
		WHEPPublicHost: lookupEnv(envWHEPPublicHost),
		WHEPPublicCert: lookupEnv(envWHEPPublicCert),
		WHEPPublicKey:  lookupEnv(envWHEPPublicKey),

		SignalPublicAddr: lookupEnv(envSignalPublicAddr),
		SignalPublicHost: lookupEnv(envSignalPublicHost),
		SignalPublicCert: lookupEnv(envSignalPublicCert),
		SignalPublicKey:  lookupEnv(envSignalPublicKey),
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
	if cfg.TURNRealm == "" {
		cfg.TURNRealm = defaultTURNRealm
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
	// Egress-token signing key (Saison 18-14): OPTIONAL. Unset is fine -
	// the /webviewer/egress-token endpoint then soft-503s (the cloud
	// egress is additive, no boot break). But if it IS set it must be
	// valid, so a typo fails fast at boot. No coupling to DIAL_URL (no
	// surprise boot-break on a deployed edge that has not set it yet).
	if c.EgressTokenHMACKey != "" {
		if _, err := c.DecodeEgressTokenHMACKey(); err != nil {
			return fmt.Errorf("config: %s invalid: %w", envEgressTokenHMACKey, err)
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

// StreamInProcessConfigured reports whether all required fields for the
// in-process stream server are present (the BaseURL is reused from
// StreamBackendURL). Only meaningful in the carvilon_stream build; the
// public build never consumes it. The tagged wiring uses this to decide
// whether to call stream.SetupEdgeInProcess: incomplete config logs and
// skips (the edge keeps running, Grundregel), full config sets it up.
func (c Config) StreamInProcessConfigured() bool {
	return c.StreamNVRHost != "" && c.StreamAPIKey != "" &&
		c.StreamDBPath != "" && c.StreamAddr != "" && c.StreamBackendURL != ""
}

// CloudStreamInProcessConfigured reports whether the cloud role should
// stand up the in-process WHIP/WHEP stream server. Soft gate (skip-or-
// start), mirroring StreamInProcessConfigured on the edge: an incomplete
// config logs and skips, so the cloud keeps running the side-channel
// only. Only meaningful in the carvilon_stream build. ValidateCloud
// enforces that, once configured, the cert/key pair is complete and the
// publish-token HMAC key is present.
func (c Config) CloudStreamInProcessConfigured() bool {
	return c.WhipCert != "" && c.WhipKey != ""
}

// CloudTURNConfigured reports whether the cloud role should stand up the
// in-process TURN relay. Soft gate (skip-or-start), INDEPENDENT of the
// WHIP/WHEP gate: true once the two mandatory fields (public IP + shared
// secret) are set. Only meaningful in the carvilon_stream build.
// ValidateCloud then checks the IP form and the optional port ranges; a
// TURN config without the cloud stream is left to a soft runtime hint in
// the (later) closure wiring, not hard-failed here.
func (c Config) CloudTURNConfigured() bool {
	return c.TURNPublicIP != "" && c.TURNSharedSecret != ""
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

// DecodeEgressTokenHMACKey hex-decodes the egress-token HMAC key and
// checks it is exactly 32 bytes (64 hex chars). Mirror of
// DecodePublishTokenHMACKey under the egress key; the separate key is
// the publish-vs-egress domain separation.
func (c Config) DecodeEgressTokenHMACKey() ([]byte, error) {
	b, err := hex.DecodeString(c.EgressTokenHMACKey)
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
	// In-process WHIP/WHEP stream (carvilon_stream build) is OPTIONAL in
	// the cloud role - the cloud may run side-channel-only. But if it is
	// partially configured, fail loudly instead of half-starting it, and
	// require the HMAC key so the ingress can verify publish tokens.
	if c.WhipCert != "" || c.WhipKey != "" {
		if c.WhipCert == "" {
			return fmt.Errorf("config: %s is required when %s is set", envWhipCert, envWhipKey)
		}
		if c.WhipKey == "" {
			return fmt.Errorf("config: %s is required when %s is set", envWhipKey, envWhipCert)
		}
		if c.PublishTokenHMACKey == "" {
			return fmt.Errorf("config: %s is required for the cloud in-process stream (%s/%s set)", envPublishTokenHMACKey, envWhipCert, envWhipKey)
		}
		if _, err := c.DecodePublishTokenHMACKey(); err != nil {
			return fmt.Errorf("config: %s invalid: %w", envPublishTokenHMACKey, err)
		}
	}
	// Egress-token signing key (Saison 18-16): OPTIONAL on the cloud role
	// too. EMPTY is allowed - the WHEP egress then fails closed (401 for
	// all, "not yet configured"). But a SET-but-invalid key is a typo
	// ("misconfigured") and must fail loudly at boot, because the VPS
	// (cloud role) is exactly where the egress key is used - otherwise it
	// would silently reject every subscriber. Mirrors the edge check in
	// Validate().
	if c.EgressTokenHMACKey != "" {
		if _, err := c.DecodeEgressTokenHMACKey(); err != nil {
			return fmt.Errorf("config: %s invalid: %w", envEgressTokenHMACKey, err)
		}
	}
	// In-process TURN relay (carvilon_stream build) is OPTIONAL and gated
	// INDEPENDENTLY of WHIP. Once configured (public IP + shared secret),
	// validate the IP form and the optional port ranges. A TURN config
	// without the cloud stream is NOT hard-failed here (side-channel-only
	// and public builds stay valid); the soft "TURN without a stream to
	// relay" hint belongs in the closure wiring. The shared secret is a
	// secret and is never echoed into an error.
	if c.CloudTURNConfigured() {
		if net.ParseIP(c.TURNPublicIP) == nil {
			return fmt.Errorf("config: %s must be a valid IP address", envTURNPublicIP)
		}
		if c.TURNUDPPort < 1 || c.TURNUDPPort > 65535 {
			return fmt.Errorf("config: %s must be in the range 1-65535", envTURNUDPPort)
		}
		// TLS relay is opt-in: 0 = OFF. Only range-check a non-zero port.
		if c.TURNTLSPort != 0 && (c.TURNTLSPort < 1 || c.TURNTLSPort > 65535) {
			return fmt.Errorf("config: %s must be 0 (TLS off) or in the range 1-65535", envTURNTLSPort)
		}
	}
	// Public WHEP egress listener (Saison 19-07 Baustufe 2): OPTIONAL, opt-in
	// via CARVILON_WHEP_PUBLIC_ADDR. Once the addr is set, the public host +
	// cert + key are ALL required - the separate listener needs the cert/key
	// and the base URL the edge advertises needs the host - so a half-config
	// fails loudly at boot rather than half-starting (mirrors the WHIP
	// cert/key pair check). Empty addr -> feature off, no validation.
	if c.WHEPPublicAddr != "" {
		if c.WHEPPublicHost == "" {
			return fmt.Errorf("config: %s is required when %s is set", envWHEPPublicHost, envWHEPPublicAddr)
		}
		if c.WHEPPublicCert == "" {
			return fmt.Errorf("config: %s is required when %s is set", envWHEPPublicCert, envWHEPPublicAddr)
		}
		if c.WHEPPublicKey == "" {
			return fmt.Errorf("config: %s is required when %s is set", envWHEPPublicKey, envWHEPPublicAddr)
		}
	}
	// Cloud control endpoint (Saison 19-11): OPTIONAL, opt-in via
	// CARVILON_SIGNAL_PUBLIC_ADDR. Once set, host + cert + key are all
	// required (the listener needs the cert/key, the logs/self-check the
	// host) - fail loud at boot, mirroring the WHEP block.
	if c.SignalPublicAddr != "" {
		if c.SignalPublicHost == "" {
			return fmt.Errorf("config: %s is required when %s is set", envSignalPublicHost, envSignalPublicAddr)
		}
		if c.SignalPublicCert == "" {
			return fmt.Errorf("config: %s is required when %s is set", envSignalPublicCert, envSignalPublicAddr)
		}
		if c.SignalPublicKey == "" {
			return fmt.Errorf("config: %s is required when %s is set", envSignalPublicKey, envSignalPublicAddr)
		}
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

// parsePort returns def when s is empty, the parsed value when s is a
// valid integer, or 0 when s is set but not a number (ValidateCloud's
// range check then rejects it loudly). Used for the optional TURN ports.
func parsePort(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}
