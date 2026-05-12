// Package magiclink issues and consumes one-shot tokens that map
// to ua_user_id values. The plain token is shown to the admin once
// and embedded into a URL handed to the tenant. Consuming the
// token (via the future http login endpoint) trades it for a
// session.
//
// Tokens are 32 random bytes encoded base64url-without-padding,
// producing 43 ASCII characters. Storage is plain text in saison
// 12; a hashing pass can be added later when a security review
// happens.
package magiclink

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"unifix.local/server/internal/db"
)

// DefaultTTL is how long a freshly created token stays valid.
const DefaultTTL = 7 * 24 * time.Hour

// Service is the magic-link issuer and consumer.
type Service struct {
	db  *db.DB
	now func() time.Time
}

// Option mutates a Service during construction. Used for
// dependency injection in tests; production code passes no
// options.
type Option func(*Service)

// WithClock replaces the default time.Now source. Tests inject
// a closure they can advance to exercise expiry paths.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// New constructs a Service backed by the given database. With
// no options the Service uses time.Now.
func New(d *db.DB, opts ...Option) *Service {
	s := &Service{db: d, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Create issues a new magic-link token for uaUserID. The token is
// returned as a 43-character base64url string. Caller is
// responsible for delivering it to the tenant (typically embedded
// in an https URL).
func (s *Service) Create(ctx context.Context, uaUserID string) (string, error) {
	if uaUserID == "" {
		return "", errors.New("magiclink: uaUserID must not be empty")
	}
	token, err := newToken()
	if err != nil {
		return "", fmt.Errorf("magiclink: generate token: %w", err)
	}
	now := s.now()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO magic_link_tokens (token, ua_user_id, created_at, expires_at)
		 VALUES (?, ?, ?, ?)`,
		token, uaUserID, now.UnixMilli(), now.Add(DefaultTTL).UnixMilli())
	if err != nil {
		return "", fmt.Errorf("magiclink: insert: %w", err)
	}
	return token, nil
}

// Consume validates the token and marks it consumed in a single
// transaction. On success it returns the ua_user_id the token was
// issued for. The error cases are exposed as sentinel values so
// callers can map them to http responses.
func (s *Service) Consume(ctx context.Context, token string) (string, error) {
	if token == "" {
		return "", ErrTokenNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("magiclink: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		uaUserID   string
		expiresAt  int64
		consumedAt sql.NullInt64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT ua_user_id, expires_at, consumed_at FROM magic_link_tokens WHERE token = ?`,
		token,
	).Scan(&uaUserID, &expiresAt, &consumedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrTokenNotFound
	}
	if err != nil {
		return "", fmt.Errorf("magiclink: select: %w", err)
	}
	if consumedAt.Valid {
		return "", ErrTokenConsumed
	}
	now := s.now().UnixMilli()
	if now > expiresAt {
		return "", ErrTokenExpired
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE magic_link_tokens SET consumed_at = ? WHERE token = ?`,
		now, token,
	); err != nil {
		return "", fmt.Errorf("magiclink: update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("magiclink: commit: %w", err)
	}
	return uaUserID, nil
}

// Sentinel errors. Callers should test for these with errors.Is.
var (
	ErrTokenNotFound = errors.New("magiclink: token not found")
	ErrTokenExpired  = errors.New("magiclink: token expired")
	ErrTokenConsumed = errors.New("magiclink: token already consumed")
)

// newToken returns 32 crypto-random bytes as base64url without
// padding, which is 43 ASCII characters.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
