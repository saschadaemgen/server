// Package adminsession owns the lifecycle of admin (Hausverwalter)
// sessions. Mirrors the mieter session package but stores rows in
// admin_sessions with a FK on admin_users instead of mock_viewers.
//
// Saison 12-06 refactor: admin sessions used to live in the
// shared sessions table with the ua_user_id surrogate
// "_admin_<username>". With sessions now hard-FK'd to mock_viewers,
// admin sessions need their own home.
//
// Session ids are 32 random bytes encoded base64url-without-
// padding (43 characters). Validate performs the same rolling
// renewal as the mieter session service.
package adminsession

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"carvilon.local/server/internal/db"
)

// DefaultIdleTimeout is the rolling expiry window on every
// successful Validate.
const DefaultIdleTimeout = 30 * 24 * time.Hour

// Meta carries optional audit context recorded with the session.
type Meta struct {
	UserAgent string
	IP        string
}

// Service operates on the admin_sessions table.
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

// Create starts a new admin session for adminUsername and
// returns the session id.
func (s *Service) Create(ctx context.Context, adminUsername string, meta Meta) (string, error) {
	if adminUsername == "" {
		return "", errors.New("adminsession: adminUsername must not be empty")
	}
	sid, err := newSessionID()
	if err != nil {
		return "", fmt.Errorf("adminsession: generate id: %w", err)
	}
	now := s.now()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO admin_sessions
		   (session_id, admin_username, created_at, last_seen, expires_at, user_agent, ip)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sid, adminUsername,
		now.UnixMilli(), now.UnixMilli(),
		now.Add(DefaultIdleTimeout).UnixMilli(),
		meta.UserAgent, meta.IP,
	)
	if err != nil {
		return "", fmt.Errorf("adminsession: insert: %w", err)
	}
	return sid, nil
}

// Validate checks a session id, renews it on success, returns
// the admin username.
func (s *Service) Validate(ctx context.Context, sessionID string) (string, error) {
	if sessionID == "" {
		return "", ErrSessionNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("adminsession: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		username  string
		expiresAt int64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT admin_username, expires_at FROM admin_sessions WHERE session_id = ?`,
		sessionID,
	).Scan(&username, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrSessionNotFound
	}
	if err != nil {
		return "", fmt.Errorf("adminsession: select: %w", err)
	}
	now := s.now()
	if now.UnixMilli() > expiresAt {
		return "", ErrSessionExpired
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE admin_sessions SET last_seen = ?, expires_at = ? WHERE session_id = ?`,
		now.UnixMilli(), now.Add(DefaultIdleTimeout).UnixMilli(), sessionID,
	); err != nil {
		return "", fmt.Errorf("adminsession: update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("adminsession: commit: %w", err)
	}
	return username, nil
}

// Revoke deletes one session. Idempotent on missing.
func (s *Service) Revoke(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM admin_sessions WHERE session_id = ?`, sessionID)
	if err != nil {
		return fmt.Errorf("adminsession: delete: %w", err)
	}
	return nil
}

// CleanupExpired drops every admin session whose expires_at has
// passed.
func (s *Service) CleanupExpired(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM admin_sessions WHERE expires_at < ?`, s.now().UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("adminsession: cleanup: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("adminsession: rows affected: %w", err)
	}
	return int(n), nil
}

// Sentinel errors.
var (
	ErrSessionNotFound = errors.New("adminsession: not found")
	ErrSessionExpired  = errors.New("adminsession: expired")
)

func newSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
