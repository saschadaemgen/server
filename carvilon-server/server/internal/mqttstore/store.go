// Package mqttstore is the persistence layer for the embedded MQTT
// broker's device credentials and ACL rules (migration 030). It is
// the single SQL writer for the mqtt_devices and mqtt_acl_rules
// tables.
//
// A "device" here is an MQTT identity, strictly separate from the
// human admin accounts (admin_users) and the viewer devices
// (viewers). Passwords are stored as Argon2id PHC strings hashed
// with the platform-wide pepper; cleartext never touches the DB,
// the logs, or any read path. The broker loads a full snapshot
// (LoadAuthz) at start and after every credential/ACL change;
// CONNECT-time verification and per-packet ACL checks run against
// that in-memory snapshot, never a live query, so this package is
// only exercised on the admin write path and at (re)load.
package mqttstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"carvilon.local/server/internal/auth/argon2id"
)

// Validation bounds for device credentials. Usernames double as the
// implicit ACL subtree segment (carvilon/<username>/#), so they are
// restricted to a topic-safe charset (no '/', '+', '#', whitespace).
const (
	MinPasswordLen = 8
	MaxUsernameLen = 64
)

var (
	// ErrDeviceExists is returned by CreateDevice for a duplicate username.
	ErrDeviceExists = errors.New("mqttstore: device already exists")
	// ErrDeviceNotFound is returned when a username has no row.
	ErrDeviceNotFound = errors.New("mqttstore: device not found")
	// ErrInvalidUsername / ErrPasswordTooShort flag rejected input.
	ErrInvalidUsername  = errors.New("mqttstore: invalid username")
	ErrPasswordTooShort = fmt.Errorf("mqttstore: password must be at least %d characters", MinPasswordLen)
	// ErrInvalidACL flags a malformed ACL rule.
	ErrInvalidACL = errors.New("mqttstore: invalid acl rule")
)

// usernameRe restricts usernames to a topic-safe charset.
var usernameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// PepperFunc returns the platform password pepper. It is called on
// the write path (hash) and at snapshot load (the snapshot caches
// the pepper for in-memory verification).
type PepperFunc func(ctx context.Context) (string, error)

// Store is the SQL gateway for MQTT device credentials and ACLs.
type Store struct {
	db     *sql.DB
	pepper PepperFunc
	now    func() time.Time
}

// Option mutates a Store during construction.
type Option func(*Store)

// WithClock injects a test clock.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// New constructs a Store. pepper must be non-nil; CreateDevice,
// SetPassword and LoadAuthz all need it.
func New(db *sql.DB, pepper PepperFunc, opts ...Option) *Store {
	s := &Store{db: db, pepper: pepper, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Device is a read view of an mqtt_devices row. The password hash is
// deliberately NOT exposed here; only LoadAuthz reads it, and only
// into the broker's in-memory snapshot.
type Device struct {
	Username      string
	Label         string
	CreatedAt     int64
	UpdatedAt     int64
	LastConnectAt int64 // 0 == never
}

// ACLRule is one row of mqtt_acl_rules.
type ACLRule struct {
	ID          int64
	Username    string
	Action      string // "publish" | "subscribe" | "both"
	TopicFilter string
	Allow       bool
}

// ValidateUsername reports whether u is an acceptable MQTT username.
func ValidateUsername(u string) error {
	if u == "" || len(u) > MaxUsernameLen || !usernameRe.MatchString(u) {
		return ErrInvalidUsername
	}
	return nil
}

// CreateDevice hashes password with the platform pepper and inserts a
// new device. Returns ErrDeviceExists on a duplicate username.
func (s *Store) CreateDevice(ctx context.Context, username, password, label string) error {
	if err := ValidateUsername(username); err != nil {
		return err
	}
	if len(password) < MinPasswordLen {
		return ErrPasswordTooShort
	}
	hash, err := s.hash(ctx, password)
	if err != nil {
		return err
	}
	now := s.now().UnixMilli()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO mqtt_devices (username, password_hash, label, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		username, hash, nullable(label), now, now,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrDeviceExists
		}
		return fmt.Errorf("mqttstore: insert device: %w", err)
	}
	return nil
}

// SetPassword rehashes and updates an existing device's password.
func (s *Store) SetPassword(ctx context.Context, username, password string) error {
	if len(password) < MinPasswordLen {
		return ErrPasswordTooShort
	}
	hash, err := s.hash(ctx, password)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE mqtt_devices SET password_hash = ?, updated_at = ? WHERE username = ?`,
		hash, s.now().UnixMilli(), username,
	)
	if err != nil {
		return fmt.Errorf("mqttstore: update password: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrDeviceNotFound
	}
	return nil
}

// DeleteDevice removes a device and (via ON DELETE CASCADE) all its
// ACL rules.
func (s *Store) DeleteDevice(ctx context.Context, username string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM mqtt_devices WHERE username = ?`, username)
	if err != nil {
		return fmt.Errorf("mqttstore: delete device: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrDeviceNotFound
	}
	return nil
}

// ListDevices returns all devices ordered by username, without hashes.
func (s *Store) ListDevices(ctx context.Context) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT username, COALESCE(label,''), created_at, updated_at, COALESCE(last_connect_at,0)
		   FROM mqtt_devices ORDER BY username`)
	if err != nil {
		return nil, fmt.Errorf("mqttstore: list devices: %w", err)
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.Username, &d.Label, &d.CreatedAt, &d.UpdatedAt, &d.LastConnectAt); err != nil {
			return nil, fmt.Errorf("mqttstore: scan device: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeviceExists reports whether a username is present.
func (s *Store) DeviceExists(ctx context.Context, username string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM mqtt_devices WHERE username = ?`, username).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("mqttstore: device exists: %w", err)
	}
	return true, nil
}

// TouchConnect records a successful CONNECT timestamp. Best-effort:
// a missing device is not an error (it may have just been deleted).
func (s *Store) TouchConnect(ctx context.Context, username string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE mqtt_devices SET last_connect_at = ? WHERE username = ?`,
		s.now().UnixMilli(), username)
	if err != nil {
		return fmt.Errorf("mqttstore: touch connect: %w", err)
	}
	return nil
}

// AddACL inserts an ACL rule for an existing device.
func (s *Store) AddACL(ctx context.Context, username, action, filter string, allow bool) error {
	if !validAction(action) || strings.TrimSpace(filter) == "" || !ValidTopicFilter(filter) {
		return ErrInvalidACL
	}
	allowInt := 0
	if allow {
		allowInt = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO mqtt_acl_rules (username, action, topic_filter, allow, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		username, action, filter, allowInt, s.now().UnixMilli())
	if err != nil {
		// A missing parent device trips the FK constraint.
		if isFKViolation(err) {
			return ErrDeviceNotFound
		}
		return fmt.Errorf("mqttstore: insert acl: %w", err)
	}
	return nil
}

// DeleteACL removes one ACL rule by id.
func (s *Store) DeleteACL(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM mqtt_acl_rules WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("mqttstore: delete acl: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrInvalidACL
	}
	return nil
}

// ListACL returns a device's ACL rules ordered by id.
func (s *Store) ListACL(ctx context.Context, username string) ([]ACLRule, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, username, action, topic_filter, allow
		   FROM mqtt_acl_rules WHERE username = ? ORDER BY id`, username)
	if err != nil {
		return nil, fmt.Errorf("mqttstore: list acl: %w", err)
	}
	defer rows.Close()
	return scanACL(rows)
}

// DeviceAuthz is the per-device slice of the broker snapshot: the
// password hash for CONNECT verification plus the ACL rules.
type DeviceAuthz struct {
	PasswordHash string
	Rules        []ACLRule
}

// Authz is the full in-memory snapshot the broker holds.
type Authz struct {
	Pepper  string
	Devices map[string]DeviceAuthz
}

// LoadAuthz reads every device + rule into a single snapshot and
// captures the current pepper. The broker calls this at start and
// after any admin change; it never queries per packet.
func (s *Store) LoadAuthz(ctx context.Context) (*Authz, error) {
	pepper, err := s.pepper(ctx)
	if err != nil {
		return nil, fmt.Errorf("mqttstore: load pepper: %w", err)
	}
	az := &Authz{Pepper: pepper, Devices: map[string]DeviceAuthz{}}

	rows, err := s.db.QueryContext(ctx, `SELECT username, password_hash FROM mqtt_devices`)
	if err != nil {
		return nil, fmt.Errorf("mqttstore: load devices: %w", err)
	}
	func() {
		defer rows.Close()
		for rows.Next() {
			var u, h string
			if err = rows.Scan(&u, &h); err != nil {
				return
			}
			az.Devices[u] = DeviceAuthz{PasswordHash: h}
		}
		err = rows.Err()
	}()
	if err != nil {
		return nil, fmt.Errorf("mqttstore: scan devices: %w", err)
	}

	arows, err := s.db.QueryContext(ctx,
		`SELECT id, username, action, topic_filter, allow FROM mqtt_acl_rules ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("mqttstore: load acl: %w", err)
	}
	defer arows.Close()
	all, err := scanACL(arows)
	if err != nil {
		return nil, err
	}
	for _, r := range all {
		d, ok := az.Devices[r.Username]
		if !ok {
			continue // orphan guard; FK should prevent this
		}
		d.Rules = append(d.Rules, r)
		az.Devices[r.Username] = d
	}
	return az, nil
}

func (s *Store) hash(ctx context.Context, password string) (string, error) {
	pepper, err := s.pepper(ctx)
	if err != nil {
		return "", fmt.Errorf("mqttstore: pepper: %w", err)
	}
	h, err := argon2id.HashWithPepper(password, pepper)
	if err != nil {
		return "", fmt.Errorf("mqttstore: hash: %w", err)
	}
	return h, nil
}

func scanACL(rows *sql.Rows) ([]ACLRule, error) {
	var out []ACLRule
	for rows.Next() {
		var r ACLRule
		var allow int
		if err := rows.Scan(&r.ID, &r.Username, &r.Action, &r.TopicFilter, &allow); err != nil {
			return nil, fmt.Errorf("mqttstore: scan acl: %w", err)
		}
		r.Allow = allow == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

func validAction(a string) bool {
	switch a {
	case "publish", "subscribe", "both":
		return true
	}
	return false
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// isUniqueViolation / isFKViolation classify modernc sqlite errors by
// message substring (the driver does not export typed codes here).
func isUniqueViolation(err error) bool {
	return strings.Contains(strings.ToUpper(err.Error()), "UNIQUE")
}

func isFKViolation(err error) bool {
	return strings.Contains(strings.ToUpper(err.Error()), "FOREIGN KEY")
}
