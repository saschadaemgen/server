// Package admin authenticates the platform operator (the
// Hausverwalter in saison 12 vocabulary). Exactly one admin user
// is supported in saison 12; a multi-admin migration is a later
// season problem.
//
// Passwords are stored as bcrypt hashes (cost 12). Login does
// not leak the difference between "user not found" and "wrong
// password" through its error type; callers should always
// surface "invalid credentials" to the browser regardless.
package admin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/bcrypt"

	"unifix.local/server/internal/db"
)

// bcryptCost is the work factor for password hashing. 12 is the
// 2024-era recommendation: balances brute-force resistance
// against login latency on the RPi (about 200 ms at cost 12).
const bcryptCost = 12

// Sentinel errors. Login uses these to distinguish between
// "credentials don't match anything" and "DB is broken"; HTTP
// handlers must map both ErrNotFound and ErrInvalidPassword to
// the same generic "invalid credentials" response.
var (
	ErrNotFound        = errors.New("admin: user not found")
	ErrInvalidPassword = errors.New("admin: invalid password")
)

// Service mediates between the admin_users table and the HTTP
// layer.
type Service struct {
	db  *db.DB
	now func() time.Time
}

// Option mutates a Service during construction.
type Option func(*Service)

// WithClock injects a test clock.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// New constructs a Service. now defaults to time.Now.
func New(d *db.DB, opts ...Option) *Service {
	s := &Service{db: d, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Login verifies username + password and bumps last_login_at on
// success. Returns ErrNotFound or ErrInvalidPassword on failure;
// the caller must not leak the distinction.
func (s *Service) Login(ctx context.Context, username, password string) error {
	var hash string
	err := s.db.QueryRowContext(ctx,
		`SELECT password_hash FROM admin_users WHERE username = ?`, username,
	).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		// Run a dummy bcrypt compare to keep timing roughly
		// constant against username-enumeration probes.
		_ = bcrypt.CompareHashAndPassword(
			[]byte("$2a$12$0000000000000000000000.0000000000000000000000000000"),
			[]byte(password),
		)
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("admin: select: %w", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return ErrInvalidPassword
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE admin_users SET last_login_at = ? WHERE username = ?`,
		s.now().UnixMilli(), username,
	); err != nil {
		return fmt.Errorf("admin: update last_login: %w", err)
	}
	return nil
}

// SetPassword creates or updates an admin user. Uses bcrypt at
// the package-level cost.
func (s *Service) SetPassword(ctx context.Context, username, password string) error {
	if username == "" {
		return errors.New("admin: username must not be empty")
	}
	if len(password) < 8 {
		return errors.New("admin: password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return fmt.Errorf("admin: bcrypt: %w", err)
	}
	now := s.now().UnixMilli()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO admin_users (username, password_hash, created_at, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(username) DO UPDATE SET
		   password_hash = excluded.password_hash,
		   updated_at = excluded.updated_at`,
		username, string(hash), now, now,
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
