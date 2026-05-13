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

	"unifix.local/server/internal/db"
	"unifix.local/server/internal/secrets"
)

// Well-known keys. Add new constants here as the platform grows.
const (
	KeyUAAPIBaseURL    = "ua_api_base_url"
	KeyUAAPIToken      = "ua_api_token"
	KeyViewerPwPepper  = "viewer_pw_pepper"
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
