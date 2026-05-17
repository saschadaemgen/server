// Package admin authenticates the platform operator (the
// Hausverwalter in saison 12 vocabulary). Exactly one admin user
// is supported in saison 12; a multi-admin migration is a later
// season problem.
//
// Saison 13-02-FIX4-a: Argon2id (m=64MB, t=3, p=4) ersetzt bcrypt
// fuer NEU gesetzte Passwoerter. Beim Login werden Bcrypt-Hashes
// transparent ueber Rehash-on-Login zu Argon2id migriert (der
// erste Login nach dem Update macht den Wechsel). Pepper wird vom
// platformconfig-Service geliefert (PepperSource interface), damit
// das admin-Paket nicht direkt von platformconfig abhaengt.
//
// Login leakt nicht den Unterschied zwischen "user not found" und
// "wrong password" durch sein Error-Type; Caller muessen beides
// als "invalid credentials" surfen.
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

// PepperSource liefert den Argon2id-Pepper zur Laufzeit. Das
// platformconfig-Paket implementiert das via GetSecret/SetSecret;
// in Tests reicht ein schlanker Stub. Empty-string return bedeutet
// "kein Pepper" und ist ein gueltiger Zustand.
type PepperSource interface {
	GetPepper(ctx context.Context) (string, error)
}

// Service mediiert zwischen der admin_users-Tabelle und der HTTP-
// Schicht.
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

// WithPepper injects the pepper source. Without it Argon2id-Hashes
// werden ohne Pepper-Konkatenation gerechnet (akzeptiert in
// Test-Setups die ohne platformconfig auskommen).
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
// last_login_at. Wenn der gespeicherte Hash bcrypt-format ist,
// wird er nach erfolgreicher Verifikation transparent zu Argon2id
// rehashed (Migrations-Pfad fuer Bestand-Admins).
func (s *Service) Login(ctx context.Context, username, password string) error {
	var hash string
	err := s.db.QueryRowContext(ctx,
		`SELECT password_hash FROM admin_users WHERE username = ?`, username,
	).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		// Dummy-Argon2id-Compare gegen einen festen Hash, um die
		// Verify-Latenz der "user found"-Pfade zu spiegeln und
		// User-Enumeration-Probes zu erschweren.
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
		// bcrypt-Pfad: rehash-on-login bei Erfolg.
		if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
			return ErrInvalidPassword
		}
		newHash, err := argon2id.HashWithPepper(password, pepper)
		if err == nil {
			if _, err := s.db.ExecContext(ctx,
				`UPDATE admin_users SET password_hash = ?, updated_at = ? WHERE username = ?`,
				newHash, s.now().UnixMilli(), username,
			); err != nil {
				// Rehash-Fehler ist nicht-fatal fuer den Login.
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

// SetPassword creates or updates an admin user. Argon2id mit Pepper.
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

// dummyArgon2idHash ist ein fester PHC-String der die normale
// Login-Latenz spiegelt um User-Enumeration zu erschweren. Die
// genaue Pepper / Salt / Hash-Bytes sind irrelevant - der Verify
// schlaegt immer fehl, aber er braucht ~250ms wie ein echter Verify.
const dummyArgon2idHash = "$argon2id$v=19$m=65536,t=3,p=4$00000000000000000000000000000000$00000000000000000000000000000000000000000000"
