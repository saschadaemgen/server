// Package platformconfig is a thin wrapper around the
// platform_config table. Plaintext values land in `value`,
// encrypted values land in `value_encrypted` after a roundtrip
// through the secrets service. Exactly one of the two columns
// is populated per row.
package platformconfig

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"carvilon.local/server/internal/db"
	"carvilon.local/server/internal/secrets"
)

// Well-known keys. Add new constants here as the platform grows.
const (
	KeyUAAPIBaseURL = "ua_api_base_url"
	KeyUAAPIToken   = "ua_api_token"
	// KeyUAEnabled ist der "UA aktiv"-Schalter der Benutzer-Seite.
	// "1"/"0". Fehlt der Wert (Erststart), gilt der Default: an, wenn
	// ein UA-Token gesetzt ist, sonst aus (siehe Server.uaEnabled).
	// CARVILONs eigene Benutzer sind davon unberuehrt - der Schalter
	// blendet nur den UA-Abschnitt der Benutzer-Seite aus.
	KeyUAEnabled      = "ua_enabled"
	KeyViewerPwPepper = "viewer_pw_pepper"
	// Saison 14-01b: physical site coordinates for the open-meteo
	// weather snapshot rendered on the mieter screensaver.
	KeyStationLat = "station_lat"
	KeyStationLon = "station_lon"
	// Saison 20: the single admin UI accent color (hex "#rrggbb").
	// One stored value drives the whole admin --accent; the later
	// setup wizard writes the same key. Empty/unset -> orange default.
	KeyAdminAccentColor = "admin_accent_color"
	// MQTT broker (step 1): admin-tunable broker on/off + ports + an
	// optional operator TLS cert/key path. Default-off: no listener
	// binds until the admin enables it. The LAN bind host is derived
	// from the server's IPv4 (not stored here); these keys hold only
	// what the admin page edits.
	KeyMQTTEnabled  = "mqtt_broker_enabled" // "1" / "0"
	KeyMQTTTCPPort  = "mqtt_tcp_port"       // default 1883
	KeyMQTTTLSPort  = "mqtt_tls_port"       // default 8883
	KeyMQTTCertFile = "mqtt_tls_cert"       // empty -> self-signed
	KeyMQTTKeyFile  = "mqtt_tls_key"        // empty -> self-signed
	// WebSocket listener for the browser MQTT console.
	KeyMQTTWSEnabled = "mqtt_ws_enabled" // "1" / "0"
	KeyMQTTWSPort    = "mqtt_ws_port"    // default 8083
	// Telegram bot: admin-toggled on/off + the bot token. The token is
	// a secret (Set/GetSecret, AES-256-GCM in value_encrypted) and is
	// never rendered back into any page - the UI only sees "gesetzt".
	// Default-off: no outbound connection until the admin enables it.
	KeyTelegramEnabled  = "telegram_enabled"   // "1" / "0"
	KeyTelegramBotToken = "telegram_bot_token" // secret (SetSecret/GetSecret)
	// Saison 21 - Protect Etappe 1: UniFi Protect Integration API
	// (rein lesende Kameras + Sensoren im Device Center). Der API-Key
	// ist ein Secret (SetSecret/GetSecret, AES-256-GCM) und erreicht
	// nie eine Seite oder ein Log - die UI sieht nur "gesetzt". Der
	// Schalter folgt dem UA-Muster: fehlt der Wert, gilt an-wenn-Key-
	// gesetzt (siehe Server.protectEnabled). Diese Config ist die
	// EINZIGE Quelle fuer den Device-Center-Pfad; der Stream-Server
	// liest seinen Key weiterhin aus seinen Env-Variablen.
	KeyProtectAPIBaseURL = "protect_api_base_url"
	KeyProtectAPIKey     = "protect_api_key" // secret (SetSecret/GetSecret)
	KeyProtectEnabled    = "protect_enabled" // "1" / "0"
	// Saison 21 - Shelly Etappe 1: Shelly-Geraete (Gen2+ lokale RPC,
	// rein lesend) als Source "Shelly" im Device Center. Die Adressen
	// sind eine kommaseparierte IPv4[:port]-Liste im LAN (mDNS-
	// Discovery ist eine spaetere Bequemlichkeit). Das optionale
	// Digest-Auth-Passwort ist ein Secret (SetSecret/GetSecret,
	// AES-256-GCM) und erreicht nie eine Seite oder ein Log - die UI
	// sieht nur "gesetzt". Der Schalter folgt dem UA/Protect-Muster:
	// fehlt der Wert, gilt an-wenn-Adressen-gesetzt (siehe
	// Server.shellyEnabled).
	KeyShellyEnabled   = "shelly_enabled"          // "1" / "0"
	KeyShellyAddresses = "shelly_device_addresses" // comma-separated IPv4[:port] (Etappe-1 legacy; seed source only)
	KeyShellyPassword  = "shelly_auth_password"    // secret (SetSecret/GetSecret)
	// Saison 21 - Shelly Etappe 2: the device set moved from the single
	// comma-separated KeyShellyAddresses string to the shelly_devices table
	// (migration 038). KeyShellyMigrated is the one-time guard so the legacy
	// list is imported into the table exactly once (and never resurrected
	// after the admin later empties it). "1" once the seed has run.
	KeyShellyMigrated = "shelly_devices_migrated" // "1" once legacy addresses were seeded
	// Saison 21 - Shelly Etappe 2b: the approval gate for mDNS discovery.
	// Default (unset/"0"): a discovered device waits as "pending approval"
	// and is never polled until the operator approves it - safe out of the
	// box, even on a flat network. "1" restores auto-adopt (a discovered
	// device becomes active immediately), for operators with a segmented
	// VLAN. Only affects NEW finds; existing pending entries are untouched.
	KeyShellyAutoAdopt = "shelly_auto_adopt" // "1" = auto-activate discovered devices; default off (gate on)
	// Saison 21 - Shelly Etappe 3, Phase 1: MQTT auto-provisioning on
	// approval. KeyShellyKeepCloud is the "keep Shelly cloud" opt-in:
	// default off (unset/"0") disables the device's cloud connection as
	// part of hardening; "1" leaves it on (Gen2+ can run cloud + our broker
	// in parallel). The broker address/CA the device is pointed at are
	// derived from the running broker (never hardcoded), not from a key.
	KeyShellyKeepCloud = "shelly_keep_cloud" // "1" = keep the Shelly cloud connection; default off
)

// Service combines the DB and the secrets service.
type Service struct {
	db      *db.DB
	secrets *secrets.Service
	now     func() time.Time
}

// Option mutates a Service during construction.
type Option func(*Service)

// WithClock injects a test clock.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// New constructs a Service. secretsSvc may be nil if the caller
// guarantees only plaintext Get/Set calls; SetSecret / GetSecret
// will then fail.
func New(d *db.DB, secretsSvc *secrets.Service, opts ...Option) *Service {
	s := &Service{db: d, secrets: secretsSvc, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// DB exposes the underlying sql.DB for callers that need to run
// ad-hoc queries (e.g. the admin dashboard reading
// schema_version). Use sparingly; prefer typed wrappers.
func (s *Service) DB() *sql.DB {
	return s.db.DB
}

// Get returns the plaintext value for key, or "" if not present.
func (s *Service) Get(ctx context.Context, key string) (string, error) {
	var v sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM platform_config WHERE key = ?`, key,
	).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("platformconfig: select: %w", err)
	}
	if !v.Valid {
		return "", nil
	}
	return v.String, nil
}

// Set stores a plaintext value. Overwrites any previous value
// (encrypted or plain) under the same key.
func (s *Service) Set(ctx context.Context, key, value string) error {
	return s.upsert(ctx, key, sql.NullString{String: value, Valid: true}, sql.NullString{})
}

// GetSecret reads value_encrypted and decrypts via the secrets
// service. Returns "" if the key is not present. Decryption
// failure is propagated.
func (s *Service) GetSecret(ctx context.Context, key string) (string, error) {
	if s.secrets == nil {
		return "", errors.New("platformconfig: secrets service required for GetSecret")
	}
	var v sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT value_encrypted FROM platform_config WHERE key = ?`, key,
	).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("platformconfig: select: %w", err)
	}
	if !v.Valid {
		return "", nil
	}
	return s.secrets.Decrypt(v.String)
}

// SetSecret encrypts value via the secrets service and stores it.
func (s *Service) SetSecret(ctx context.Context, key, value string) error {
	if s.secrets == nil {
		return errors.New("platformconfig: secrets service required for SetSecret")
	}
	enc, err := s.secrets.Encrypt(value)
	if err != nil {
		return fmt.Errorf("platformconfig: encrypt: %w", err)
	}
	return s.upsert(ctx, key, sql.NullString{}, sql.NullString{String: enc, Valid: true})
}

// Delete removes the row for key.
func (s *Service) Delete(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM platform_config WHERE key = ?`, key)
	if err != nil {
		return fmt.Errorf("platformconfig: delete: %w", err)
	}
	return nil
}

func (s *Service) upsert(ctx context.Context, key string, value, encrypted sql.NullString) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO platform_config (key, value, value_encrypted, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET
		   value = excluded.value,
		   value_encrypted = excluded.value_encrypted,
		   updated_at = excluded.updated_at`,
		key, value, encrypted, s.now().UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("platformconfig: upsert: %w", err)
	}
	return nil
}
