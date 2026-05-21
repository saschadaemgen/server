// Package admin authenticates the platform operator (Hausverwalter
// in the German UI). Exactly one admin user is supported for now;
// a multi-admin migration is a later-season problem.
//
// Argon2id (m=64MB, t=3, p=4) replaces bcrypt for newly set
// passwords. On login, bcrypt hashes are transparently rehashed to
// Argon2id (the first login after the update performs the swap).
// The pepper is supplied by platformconfig via the PepperSource
// interface so the admin package does not depend on platformconfig
// directly.
//
// Login never leaks the difference between "user not found" and
// "wrong password" through its error type; callers must surface
// both as the same generic "invalid credentials" response.
package admin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/bcrypt"

	"carvilon.local/server/internal/auth/argon2id"
	"carvilon.local/server/internal/db"
)

// Sentinel errors. Login uses these to distinguish between
// "credentials don't match anything" and "DB is broken"; HTTP
// handlers must map both ErrNotFound and ErrInvalidPassword to
// the same generic "invalid credentials" response.
var (
	ErrNotFound         = errors.New("admin: user not found")
	ErrInvalidPassword  = errors.New("admin: invalid password")
	ErrPasswordTooShort = errors.New("admin: password must be at least 12 characters")
)

// PepperSource provides the Argon2id pepper at runtime. The
// platformconfig package implements it via GetSecret/SetSecret;
// a thin stub is enough for tests. An empty-string return means
// "no pepper" and is a valid state.
type PepperSource interface {
	GetPepper(ctx context.Context) (string, error)
}

// Service mediates between the admin_users table and the HTTP
// layer.
type Service struct {
	db     *db.DB
	pepper PepperSource
	now    func() time.Time
}

// Option mutates a Service during construction.
type Option func(*Service)

// WithClock injects a test clock.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// WithPepper injects the pepper source. Without it, Argon2id
// hashes are computed without pepper concatenation (accepted in
// test setups that run without platformconfig).
func WithPepper(p PepperSource) Option {
	return func(s *Service) { s.pepper = p }
}

// New constructs a Service. now defaults to time.Now.
func New(d *db.DB, opts ...Option) *Service {
	s := &Service{db: d, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Login verifies username + password. On success bumps
// last_login_at. If the stored hash is in bcrypt format, it is
// transparently rehashed to Argon2id after a successful verify
// (migration path for legacy admins).
func (s *Service) Login(ctx context.Context, username, password string) error {
	var hash string
	err := s.db.QueryRowContext(ctx,
		`SELECT password_hash FROM admin_users WHERE username = ?`, username,
	).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		// Dummy Argon2id compare against a fixed hash to mirror the
		// verify latency of the "user found" paths and frustrate
		// user-enumeration probes.
		_, _ = argon2id.VerifyWithPepper(password, "", dummyArgon2idHash)
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("admin: select: %w", err)
	}

	pepper, _ := s.fetchPepper(ctx)

	switch {
	case argon2id.LooksLikeArgon2id(hash):
		ok, err := argon2id.VerifyWithPepper(password, pepper, hash)
		if err != nil {
			return fmt.Errorf("admin: verify argon2id: %w", err)
		}
		if !ok {
			return ErrInvalidPassword
		}
	default:
		// bcrypt path: rehash on successful login.
		if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
			return ErrInvalidPassword
		}
		newHash, err := argon2id.HashWithPepper(password, pepper)
		if err == nil {
			if _, err := s.db.ExecContext(ctx,
				`UPDATE admin_users SET password_hash = ?, updated_at = ? WHERE username = ?`,
				newHash, s.now().UnixMilli(), username,
			); err != nil {
				// Rehash failure is non-fatal for the login.
			}
		}
	}

	if _, err := s.db.ExecContext(ctx,
		`UPDATE admin_users SET last_login_at = ? WHERE username = ?`,
		s.now().UnixMilli(), username,
	); err != nil {
		return fmt.Errorf("admin: update last_login: %w", err)
	}
	return nil
}

// SetPassword creates or updates an admin user, hashing with
// Argon2id + pepper.
func (s *Service) SetPassword(ctx context.Context, username, password string) error {
	if username == "" {
		return errors.New("admin: username must not be empty")
	}
	if len(password) < 12 {
		return ErrPasswordTooShort
	}
	pepper, _ := s.fetchPepper(ctx)
	hash, err := argon2id.HashWithPepper(password, pepper)
	if err != nil {
		return fmt.Errorf("admin: hash: %w", err)
	}
	now := s.now().UnixMilli()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO admin_users (username, password_hash, created_at, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(username) DO UPDATE SET
		   password_hash = excluded.password_hash,
		   updated_at = excluded.updated_at`,
		username, hash, now, now,
	)
	if err != nil {
		return fmt.Errorf("admin: upsert: %w", err)
	}
	return nil
}

// Exists reports whether at least one admin user is present.
// The /a/login handler uses this to switch between the setup
// flow and the regular login form.
func (s *Service) Exists(ctx context.Context) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM admin_users`,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("admin: count: %w", err)
	}
	return n > 0, nil
}

func (s *Service) fetchPepper(ctx context.Context) (string, error) {
	if s.pepper == nil {
		return "", nil
	}
	return s.pepper.GetPepper(ctx)
}

// dummyArgon2idHash is a fixed PHC string used to mirror the real
// login latency and thereby frustrate user enumeration. The exact
// pepper / salt / hash bytes are irrelevant - verify always fails,
// but it takes ~250ms like a real verify.
const dummyArgon2idHash = "$argon2id$v=19$m=65536,t=3,p=4$00000000000000000000000000000000$00000000000000000000000000000000000000000000"
