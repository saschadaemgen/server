// Package session owns the lifecycle of authenticated tenant
// sessions. A session is created when a magic-link token is
// consumed and is validated on every subsequent request. Each
// validate call performs a rolling renewal: last_seen is bumped
// and expires_at is pushed out by DefaultIdleTimeout.
//
// Session ids are 32 random bytes encoded base64url-without-
// padding (43 characters). They live in the sessions table keyed
// by ua_user_id; revoking by ua_user_id wipes every active
// session for that tenant.
package session

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

// DefaultIdleTimeout is how far in the future expires_at is
// pushed by Create and every successful Validate.
const DefaultIdleTimeout = 30 * 24 * time.Hour

// Meta carries optional context recorded with the session for
// auditing. Empty values are fine.
type Meta struct {
	UserAgent string
	IP        string
}

// Service manages session lifecycle against the sessions table.
type Service struct {
	db  *db.DB
	now func() time.Time
}

// New constructs a Service. now defaults to time.Now; tests may
// override the field directly.
func New(d *db.DB) *Service {
	return &Service{db: d, now: time.Now}
}

// Create starts a new session for uaUserID and returns the
// session id. expires_at is set to now + DefaultIdleTimeout.
func (s *Service) Create(ctx context.Context, uaUserID string, meta Meta) (string, error) {
	if uaUserID == "" {
		return "", errors.New("session: uaUserID must not be empty")
	}
	sid, err := newSessionID()
	if err != nil {
		return "", fmt.Errorf("session: generate id: %w", err)
	}
	now := s.now()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO sessions
		   (session_id, ua_user_id, created_at, last_seen, expires_at, user_agent, ip)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sid, uaUserID,
		now.UnixMilli(), now.UnixMilli(),
		now.Add(DefaultIdleTimeout).UnixMilli(),
		meta.UserAgent, meta.IP,
	)
	if err != nil {
		return "", fmt.Errorf("session: insert: %w", err)
	}
	return sid, nil
}

// Validate checks a session id and renews it on success. On a
// hit, last_seen and expires_at are updated in the same
// transaction so concurrent validates cannot race past each
// other. Returns the ua_user_id the session belongs to.
func (s *Service) Validate(ctx context.Context, sessionID string) (string, error) {
	if sessionID == "" {
		return "", ErrSessionNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("session: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		uaUserID  string
		expiresAt int64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT ua_user_id, expires_at FROM sessions WHERE session_id = ?`,
		sessionID,
	).Scan(&uaUserID, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrSessionNotFound
	}
	if err != nil {
		return "", fmt.Errorf("session: select: %w", err)
	}
	now := s.now()
	if now.UnixMilli() > expiresAt {
		return "", ErrSessionExpired
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET last_seen = ?, expires_at = ? WHERE session_id = ?`,
		now.UnixMilli(), now.Add(DefaultIdleTimeout).UnixMilli(), sessionID,
	); err != nil {
		return "", fmt.Errorf("session: update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("session: commit: %w", err)
	}
	return uaUserID, nil
}

// Revoke deletes a single session. Missing sessions are not an
// error: revoke is idempotent.
func (s *Service) Revoke(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE session_id = ?`, sessionID)
	if err != nil {
		return fmt.Errorf("session: delete: %w", err)
	}
	return nil
}

// RevokeAll deletes every session for uaUserID and returns the
// number of rows removed. Useful when an admin deactivates a
// tenant.
func (s *Service) RevokeAll(ctx context.Context, uaUserID string) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE ua_user_id = ?`, uaUserID)
	if err != nil {
		return 0, fmt.Errorf("session: delete by user: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("session: rows affected: %w", err)
	}
	return int(n), nil
}

// CleanupExpired removes every session whose expires_at is in the
// past and returns the count. Intended to be called periodically
// by a future background job; in saison 12 it is implemented but
// not yet wired up.
func (s *Service) CleanupExpired(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE expires_at < ?`, s.now().UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("session: cleanup: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("session: rows affected: %w", err)
	}
	return int(n), nil
}

// Sentinel errors. Callers should test for these with errors.Is.
var (
	ErrSessionNotFound = errors.New("session: not found")
	ErrSessionExpired  = errors.New("session: expired")
)

// newSessionID returns 32 crypto-random bytes as base64url
// without padding (43 ASCII characters).
func newSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
