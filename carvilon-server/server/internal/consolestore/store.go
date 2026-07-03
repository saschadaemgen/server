// Package consolestore is the persistence layer for the terminal dock's
// saved connection profiles and its trust-on-first-use (TOFU) host-key
// pins (migration 033). It is the single SQL writer for the
// console_profiles and console_host_keys tables.
//
// Secrets never leave in the clear: a profile's password or private key
// (and an optional key passphrase) are stored as AES-256-GCM ciphertext
// via internal/secrets. The list/read API returns only "is a secret
// set" flags — never the plaintext, mirroring how the Telegram bot token
// is handled. A separate, deliberately internal accessor
// (ProfileCredential) decrypts on demand for the moment a connection is
// actually dialled.
package consolestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"carvilon.local/server/internal/secrets"
)

var (
	// ErrNotFound is returned when a profile id has no row.
	ErrNotFound = errors.New("consolestore: not found")
	// ErrEmptyName is returned when a profile name is blank after trimming.
	ErrEmptyName = errors.New("consolestore: name must not be empty")
	// ErrEmptyHost is returned when a profile host is blank.
	ErrEmptyHost = errors.New("consolestore: host must not be empty")
	// ErrBadAuthKind is returned for an auth kind other than password/key.
	ErrBadAuthKind = errors.New("consolestore: auth kind must be 'password' or 'key'")
	// ErrNoSecret is returned by ProfileCredential when the profile has
	// no stored secret to decrypt (e.g. never set on create).
	ErrNoSecret = errors.New("consolestore: profile has no stored secret")
)

const (
	// AuthPassword / AuthKey are the two supported auth kinds.
	AuthPassword = "password"
	AuthKey      = "key"

	maxNameLen = 120
	maxHostLen = 255
)

// Store is the SQL gateway for console profiles and host-key pins.
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

// New constructs a Store. The secrets service encrypts and decrypts the
// stored credentials; it must not be nil.
func New(db *sql.DB, sec *secrets.Service, opts ...Option) *Store {
	s := &Store{db: db, secrets: sec, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Profile is the metadata view of one saved connection. It never carries
// the plaintext secret — only HasSecret/HasPassphrase flags.
type Profile struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	Username      string `json:"username"`
	AuthKind      string `json:"auth_kind"`
	HasSecret     bool   `json:"has_secret"`
	HasPassphrase bool   `json:"has_passphrase"`
	Sort          int64  `json:"sort"`
	UpdatedAt     int64  `json:"updated_at"`
}

// ProfileInput is the create/update payload. Empty Secret on update keeps
// the stored secret untouched; a non-empty Secret replaces it. Passphrase
// follows the same rule but only matters for AuthKey. ClearPassphrase
// forces the stored passphrase to be dropped on update.
type ProfileInput struct {
	Name            string
	Host            string
	Port            int
	Username        string
	AuthKind        string
	Secret          string // password or private-key PEM; "" = keep on update
	Passphrase      string // optional key passphrase; "" = keep on update
	ClearPassphrase bool
}

// Credential is the decrypted secret set for one profile, handed to the
// SSH dialler at connect time and never serialized or logged.
type Credential struct {
	AuthKind   string
	Password   string
	PrivateKey []byte
	Passphrase string
}

func (in ProfileInput) validate() error {
	if strings.TrimSpace(in.Name) == "" {
		return ErrEmptyName
	}
	if strings.TrimSpace(in.Host) == "" {
		return ErrEmptyHost
	}
	if in.AuthKind != AuthPassword && in.AuthKind != AuthKey {
		return ErrBadAuthKind
	}
	return nil
}

// clampPort keeps the port in the TCP range, defaulting a zero/invalid
// value to 22 (SSH).
func clampPort(p int) int {
	if p <= 0 || p > 65535 {
		return 22
	}
	return p
}

func clamp(s string, n int) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) > n {
		s = string([]rune(s)[:n])
	}
	return s
}

// CreateProfile inserts a new profile, encrypting its secret. The secret
// must be present on create (a profile with no credential could not
// connect). Returns the stored metadata.
func (s *Store) CreateProfile(ctx context.Context, in ProfileInput) (Profile, error) {
	if err := in.validate(); err != nil {
		return Profile{}, err
	}
	if in.Secret == "" {
		return Profile{}, ErrNoSecret
	}
	secretEnc, err := s.secrets.Encrypt(in.Secret)
	if err != nil {
		return Profile{}, fmt.Errorf("consolestore: encrypt secret: %w", err)
	}
	passEnc := ""
	if in.AuthKind == AuthKey && in.Passphrase != "" {
		passEnc, err = s.secrets.Encrypt(in.Passphrase)
		if err != nil {
			return Profile{}, fmt.Errorf("consolestore: encrypt passphrase: %w", err)
		}
	}
	name, host := clamp(in.Name, maxNameLen), clamp(in.Host, maxHostLen)
	user := clamp(in.Username, maxHostLen)
	port := clampPort(in.Port)
	now := s.now().UnixMilli()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Profile{}, fmt.Errorf("consolestore: begin create: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var sort int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(sort)+1, 0) FROM console_profiles`).Scan(&sort); err != nil {
		return Profile{}, fmt.Errorf("consolestore: next sort: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO console_profiles
		   (name, host, port, username, auth_kind, secret_enc, passphrase_enc, sort, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		name, host, port, user, in.AuthKind, secretEnc, passEnc, sort, now, now)
	if err != nil {
		return Profile{}, fmt.Errorf("consolestore: insert profile: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Profile{}, fmt.Errorf("consolestore: profile id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Profile{}, fmt.Errorf("consolestore: commit create: %w", err)
	}
	return Profile{
		ID: id, Name: name, Host: host, Port: port, Username: user,
		AuthKind: in.AuthKind, HasSecret: true, HasPassphrase: passEnc != "",
		Sort: sort, UpdatedAt: now,
	}, nil
}

// UpdateProfile edits a profile. An empty Secret leaves the stored
// credential untouched (so the UI never needs to re-enter it); a
// non-empty Secret replaces it. Passphrase follows the same keep-on-empty
// rule unless ClearPassphrase is set.
func (s *Store) UpdateProfile(ctx context.Context, id int64, in ProfileInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	name, host := clamp(in.Name, maxNameLen), clamp(in.Host, maxHostLen)
	user := clamp(in.Username, maxHostLen)
	port := clampPort(in.Port)
	now := s.now().UnixMilli()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("consolestore: begin update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var exists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM console_profiles WHERE id = ?`, id).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("consolestore: read profile: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE console_profiles
		    SET name = ?, host = ?, port = ?, username = ?, auth_kind = ?, updated_at = ?
		  WHERE id = ?`,
		name, host, port, user, in.AuthKind, now, id); err != nil {
		return fmt.Errorf("consolestore: update profile: %w", err)
	}
	if in.Secret != "" {
		secretEnc, err := s.secrets.Encrypt(in.Secret)
		if err != nil {
			return fmt.Errorf("consolestore: encrypt secret: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE console_profiles SET secret_enc = ? WHERE id = ?`, secretEnc, id); err != nil {
			return fmt.Errorf("consolestore: store secret: %w", err)
		}
	}
	switch {
	case in.ClearPassphrase:
		if _, err := tx.ExecContext(ctx,
			`UPDATE console_profiles SET passphrase_enc = '' WHERE id = ?`, id); err != nil {
			return fmt.Errorf("consolestore: clear passphrase: %w", err)
		}
	case in.Passphrase != "":
		passEnc, err := s.secrets.Encrypt(in.Passphrase)
		if err != nil {
			return fmt.Errorf("consolestore: encrypt passphrase: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE console_profiles SET passphrase_enc = ? WHERE id = ?`, passEnc, id); err != nil {
			return fmt.Errorf("consolestore: store passphrase: %w", err)
		}
	}
	return tx.Commit()
}

// DeleteProfile removes a profile. Deleting a missing id is a no-op.
func (s *Store) DeleteProfile(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM console_profiles WHERE id = ?`, id); err != nil {
		return fmt.Errorf("consolestore: delete profile: %w", err)
	}
	return nil
}

// ListProfiles returns every profile's metadata (no secrets), ordered by
// sort then name then id.
func (s *Store) ListProfiles(ctx context.Context) ([]Profile, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, host, port, username, auth_kind,
		        secret_enc <> '', passphrase_enc <> '', sort, updated_at
		   FROM console_profiles
		  ORDER BY sort, name COLLATE NOCASE, id`)
	if err != nil {
		return nil, fmt.Errorf("consolestore: list profiles: %w", err)
	}
	defer rows.Close()
	var out []Profile
	for rows.Next() {
		var p Profile
		if err := rows.Scan(&p.ID, &p.Name, &p.Host, &p.Port, &p.Username,
			&p.AuthKind, &p.HasSecret, &p.HasPassphrase, &p.Sort, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("consolestore: scan profile: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProfile returns one profile's metadata (no secret).
func (s *Store) GetProfile(ctx context.Context, id int64) (Profile, error) {
	var p Profile
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, host, port, username, auth_kind,
		        secret_enc <> '', passphrase_enc <> '', sort, updated_at
		   FROM console_profiles WHERE id = ?`, id).
		Scan(&p.ID, &p.Name, &p.Host, &p.Port, &p.Username,
			&p.AuthKind, &p.HasSecret, &p.HasPassphrase, &p.Sort, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, ErrNotFound
	}
	if err != nil {
		return Profile{}, fmt.Errorf("consolestore: get profile: %w", err)
	}
	return p, nil
}

// ProfileCredential decrypts a profile's stored secret for the moment a
// connection is dialled. The result is never serialized or logged; the
// caller uses it and drops it. ErrNoSecret when the profile stored none.
func (s *Store) ProfileCredential(ctx context.Context, id int64) (Credential, error) {
	var (
		authKind  string
		secretEnc string
		passEnc   string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT auth_kind, secret_enc, passphrase_enc
		   FROM console_profiles WHERE id = ?`, id).
		Scan(&authKind, &secretEnc, &passEnc)
	if errors.Is(err, sql.ErrNoRows) {
		return Credential{}, ErrNotFound
	}
	if err != nil {
		return Credential{}, fmt.Errorf("consolestore: read credential: %w", err)
	}
	if secretEnc == "" {
		return Credential{}, ErrNoSecret
	}
	secret, err := s.secrets.Decrypt(secretEnc)
	if err != nil {
		return Credential{}, fmt.Errorf("consolestore: decrypt secret: %w", err)
	}
	cred := Credential{AuthKind: authKind}
	if authKind == AuthKey {
		cred.PrivateKey = []byte(secret)
		if passEnc != "" {
			pass, err := s.secrets.Decrypt(passEnc)
			if err != nil {
				return Credential{}, fmt.Errorf("consolestore: decrypt passphrase: %w", err)
			}
			cred.Passphrase = pass
		}
	} else {
		cred.Password = secret
	}
	return cred, nil
}

// ---------- host-key TOFU ----------

// LookupHostKey returns the pinned fingerprint for hostPort, or "" when
// nothing is pinned yet (first use).
func (s *Store) LookupHostKey(ctx context.Context, hostPort string) (string, error) {
	var fp string
	err := s.db.QueryRowContext(ctx,
		`SELECT fingerprint FROM console_host_keys WHERE host_port = ?`, hostPort).Scan(&fp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("consolestore: lookup host key: %w", err)
	}
	return fp, nil
}

// PinHostKey stores (or replaces) the trusted fingerprint for hostPort.
// Replacing is how an explicit re-trust after a host-key change is
// recorded.
func (s *Store) PinHostKey(ctx context.Context, hostPort, keyType, fingerprint string) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO console_host_keys (host_port, key_type, fingerprint, added_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(host_port) DO UPDATE SET
		   key_type = excluded.key_type,
		   fingerprint = excluded.fingerprint,
		   added_at = excluded.added_at`,
		hostPort, keyType, fingerprint, s.now().UnixMilli()); err != nil {
		return fmt.Errorf("consolestore: pin host key: %w", err)
	}
	return nil
}

// ForgetHostKey removes any pin for hostPort (used to explicitly re-arm
// TOFU when the operator decides to re-trust from scratch).
func (s *Store) ForgetHostKey(ctx context.Context, hostPort string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM console_host_keys WHERE host_port = ?`, hostPort); err != nil {
		return fmt.Errorf("consolestore: forget host key: %w", err)
	}
	return nil
}
