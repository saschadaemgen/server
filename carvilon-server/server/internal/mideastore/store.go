// Package mideastore is the persistence layer for the Midea Climate Controller
// device set (migration 042): the single source of truth for which Midea
// split-AC units CARVILON has discovered, adopted or ignored, plus their
// encrypted V3 credentials.
//
// It mirrors shellystore's two-axis model (origin + lifecycle state) and adds a
// third piece Shelly does not need: the device credentials. A Midea V3 unit is
// only controllable with a token+key that are negotiated once with the Midea
// cloud (or imported); we persist them AES-256-GCM encrypted (internal/secrets)
// in the *_enc columns so an adopted device survives a server restart and is
// re-provisioned locally without touching the cloud again. Credentials are
// never returned by the list APIs (only a HasCreds flag) and never stored in
// plaintext.
//
// Identity is the native Midea device id (hex), which is stable across a DHCP
// address change - the approval gate and the sticky ignore list both key on it.
// Nothing here ever talks to a device; provisioning and control live in
// internal/mideamonitor.
package mideastore

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"carvilon.local/server/internal/secrets"
)

// ErrNotFound is returned when an id has no matching row.
var ErrNotFound = errors.New("mideastore: device not found")

// ErrNoSecrets is returned when a credential operation is attempted without a
// secrets service (encryption is mandatory for credential material).
var ErrNoSecrets = errors.New("mideastore: secrets service required for credentials")

// Lifecycle states and origins as stored in the table.
const (
	// StatePending is a locally discovered device that has NOT been contacted:
	// a stored record only, no credentials, never polled. It waits for the
	// operator to Approve (-> active) or Reject (-> ignored).
	StatePending = "pending"
	// StateActive is an adopted device: credentials present, provisioned,
	// polled and controllable.
	StateActive = "active"
	// StateIgnored is a sticky-removed / rejected device: kept so discovery
	// recognises it by id and does not re-adopt it until released.
	StateIgnored = "ignored"

	OriginDiscovered = "discovered"
	OriginManual     = "manual"

	// ProfileStandard is device-side control (remote-like passthrough, the E1
	// default). ProfileAdvanced is the server-side control loop, unlocked in a
	// later etappe; the column carries the toggle either way.
	ProfileStandard = "standard"
	ProfileAdvanced = "advanced"
)

// Store is the SQL gateway for the Midea device set.
type Store struct {
	db      *sql.DB
	secrets *secrets.Service
	now     func() time.Time
}

// Option mutates a Store during construction.
type Option func(*Store)

// WithClock injects a test clock.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// New constructs a Store. sec may be nil (credential operations then error),
// which keeps the discovery/list paths usable in tests without a key.
func New(db *sql.DB, sec *secrets.Service, opts ...Option) *Store {
	s := &Store{db: db, secrets: sec, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Device is one row of the Midea device set (never carries plaintext creds).
type Device struct {
	ID          string // stable identity: lowercase hex of DeviceID
	DeviceID    uint64
	Address     string
	Name        string
	ProtocolV3  bool
	Origin      string // OriginDiscovered | OriginManual
	State       string // StatePending | StateActive | StateIgnored
	Profile     string // ProfileStandard | ProfileAdvanced
	HasCreds    bool
	FirstSeenAt int64 // ms epoch
	UpdatedAt   int64 // ms epoch
}

// Detected is one device local discovery reported, in the neutral shape
// InsertDiscovered understands.
type Detected struct {
	DeviceID   uint64
	Address    string
	Name       string
	ProtocolV3 bool
	Origin     string // OriginDiscovered (default, "") | OriginManual
}

// IDFor returns the stable string id for a native Midea device id.
func IDFor(deviceID uint64) string { return fmt.Sprintf("%x", deviceID) }

func (s *Store) nowMS() int64 { return s.now().UnixMilli() }

// InsertDiscovered upserts a locally discovered device. A brand-new device is
// inserted as pending; an existing one has its last-seen address/name/version
// refreshed but its lifecycle state is NEVER changed here (a sticky-ignored
// device stays ignored, an active one stays active). Returns the resulting
// stored device.
func (s *Store) InsertDiscovered(ctx context.Context, d Detected) (Device, error) {
	if d.DeviceID == 0 {
		return Device{}, errors.New("mideastore: device id required")
	}
	id := IDFor(d.DeviceID)
	origin := d.Origin
	if origin == "" {
		origin = OriginDiscovered
	}
	now := s.nowMS()
	v3 := 0
	if d.ProtocolV3 {
		v3 = 1
	}
	// Upsert: keep state/profile/origin/creds on conflict, refresh last-seen.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO midea_devices
		    (id, device_id, address, name, protocol_v3, origin, state, profile, first_seen_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		    address     = excluded.address,
		    name        = CASE WHEN excluded.name <> '' THEN excluded.name ELSE midea_devices.name END,
		    protocol_v3 = excluded.protocol_v3,
		    updated_at  = excluded.updated_at`,
		id, int64(d.DeviceID), d.Address, d.Name, v3, origin, StatePending, ProfileStandard, now, now)
	if err != nil {
		return Device{}, fmt.Errorf("mideastore: insert discovered: %w", err)
	}
	return s.Get(ctx, id)
}

// AddManual inserts (or refreshes) a manually targeted device as pending. Used
// by a targeted-IP discovery so a device behind a VLAN boundary can be adopted.
func (s *Store) AddManual(ctx context.Context, d Detected) (Device, error) {
	d.Origin = OriginManual
	return s.InsertDiscovered(ctx, d)
}

const selectCols = `id, device_id, address, name, protocol_v3, origin, state, profile,
	(token_enc <> '' AND key_enc <> '') AS has_creds, first_seen_at, updated_at`

func (s *Store) query(ctx context.Context, where string, args ...any) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+selectCols+` FROM midea_devices `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("mideastore: query: %w", err)
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanDevice(sc scanner) (Device, error) {
	var (
		d      Device
		devID  int64
		v3, hc int
	)
	if err := sc.Scan(&d.ID, &devID, &d.Address, &d.Name, &v3, &d.Origin, &d.State, &d.Profile,
		&hc, &d.FirstSeenAt, &d.UpdatedAt); err != nil {
		return Device{}, fmt.Errorf("mideastore: scan: %w", err)
	}
	d.DeviceID = uint64(devID)
	d.ProtocolV3 = v3 != 0
	d.HasCreds = hc != 0
	return d, nil
}

// ListActive returns adopted devices (polled + controllable), stable order.
func (s *Store) ListActive(ctx context.Context) ([]Device, error) {
	return s.query(ctx, `WHERE state = ? ORDER BY name, id`, StateActive)
}

// ListPending returns discovered-but-not-adopted devices.
func (s *Store) ListPending(ctx context.Context) ([]Device, error) {
	return s.query(ctx, `WHERE state = ? ORDER BY name, id`, StatePending)
}

// ListIgnored returns sticky-removed / rejected devices.
func (s *Store) ListIgnored(ctx context.Context) ([]Device, error) {
	return s.query(ctx, `WHERE state = ? ORDER BY name, id`, StateIgnored)
}

// Get returns one device by id.
func (s *Store) Get(ctx context.Context, id string) (Device, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+selectCols+` FROM midea_devices WHERE id = ?`, id)
	d, err := scanDevice(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrNotFound
	}
	return d, err
}

// CountActive / CountPending back the facet counts.
func (s *Store) CountActive(ctx context.Context) (int, error)  { return s.count(ctx, StateActive) }
func (s *Store) CountPending(ctx context.Context) (int, error) { return s.count(ctx, StatePending) }

func (s *Store) count(ctx context.Context, state string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM midea_devices WHERE state = ?`, state).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("mideastore: count: %w", err)
	}
	return n, nil
}

// Approve adopts a pending device: it persists the (already fetched + locally
// verified) credentials encrypted, sets the device active and records the
// chosen profile. token/key are the raw credential bytes.
func (s *Store) Approve(ctx context.Context, id string, token, key []byte, profile string) error {
	if s.secrets == nil {
		return ErrNoSecrets
	}
	if len(token) == 0 || len(key) == 0 {
		return errors.New("mideastore: empty credentials")
	}
	if profile != ProfileAdvanced {
		profile = ProfileStandard
	}
	tokenEnc, err := s.secrets.Encrypt(hex.EncodeToString(token))
	if err != nil {
		return fmt.Errorf("mideastore: encrypt token: %w", err)
	}
	keyEnc, err := s.secrets.Encrypt(hex.EncodeToString(key))
	if err != nil {
		return fmt.Errorf("mideastore: encrypt key: %w", err)
	}
	// Guard on state = 'pending' so an approval whose cloud-pairing window
	// raced a concurrent Reject (device now 'ignored') affects 0 rows and
	// returns ErrNotFound instead of silently un-ignoring it. Mirrors Reject
	// and Shelly's ApprovePending.
	res, err := s.db.ExecContext(ctx, `
		UPDATE midea_devices
		   SET state = ?, profile = ?, token_enc = ?, key_enc = ?, updated_at = ?
		 WHERE id = ? AND state = ?`,
		StateActive, profile, tokenEnc, keyEnc, s.nowMS(), id, StatePending)
	return oneRow(res, err)
}

// Reject sends a pending device to the sticky ignore list.
func (s *Store) Reject(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE midea_devices SET state = ?, updated_at = ? WHERE id = ? AND state = ?`,
		StateIgnored, s.nowMS(), id, StatePending)
	return oneRow(res, err)
}

// Release un-ignores a device by deleting its row, so discovery can surface it
// again (and, if it is re-approved, fetch fresh credentials).
func (s *Store) Release(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM midea_devices WHERE id = ? AND state = ?`, id, StateIgnored)
	return oneRow(res, err)
}

// Remove forgets an active (or any) device entirely, dropping its stored
// credentials. The device itself is never written to.
func (s *Store) Remove(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM midea_devices WHERE id = ?`, id)
	return oneRow(res, err)
}

// SetProfile switches an adopted device between standard and advanced.
func (s *Store) SetProfile(ctx context.Context, id, profile string) error {
	if profile != ProfileAdvanced {
		profile = ProfileStandard
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE midea_devices SET profile = ?, updated_at = ? WHERE id = ?`, profile, s.nowMS(), id)
	return oneRow(res, err)
}

// Credential decrypts and returns the raw token+key for an adopted device.
func (s *Store) Credential(ctx context.Context, id string) (token, key []byte, err error) {
	if s.secrets == nil {
		return nil, nil, ErrNoSecrets
	}
	var tokenEnc, keyEnc string
	row := s.db.QueryRowContext(ctx, `SELECT token_enc, key_enc FROM midea_devices WHERE id = ?`, id)
	if err := row.Scan(&tokenEnc, &keyEnc); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, fmt.Errorf("mideastore: select creds: %w", err)
	}
	if tokenEnc == "" || keyEnc == "" {
		return nil, nil, ErrNotFound
	}
	tokenHex, err := s.secrets.Decrypt(tokenEnc)
	if err != nil {
		return nil, nil, fmt.Errorf("mideastore: decrypt token: %w", err)
	}
	keyHex, err := s.secrets.Decrypt(keyEnc)
	if err != nil {
		return nil, nil, fmt.Errorf("mideastore: decrypt key: %w", err)
	}
	token, err = hex.DecodeString(strings.TrimSpace(tokenHex))
	if err != nil {
		return nil, nil, fmt.Errorf("mideastore: token not hex: %w", err)
	}
	key, err = hex.DecodeString(strings.TrimSpace(keyHex))
	if err != nil {
		return nil, nil, fmt.Errorf("mideastore: key not hex: %w", err)
	}
	return token, key, nil
}

func oneRow(res sql.Result, err error) error {
	if err != nil {
		return fmt.Errorf("mideastore: exec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("mideastore: rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
