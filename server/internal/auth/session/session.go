// Package session owns the lifecycle of authenticated viewer
// sessions (the tenant browsers behind /m/). A session is created
// when a viewer logs in with username + password, and is validated
// on every subsequent request. Each Validate call performs a
// rolling renewal: last_seen is bumped and expires_at is pushed
// out by DefaultIdleTimeout.
//
// Saison 13-02-FIX4-a: the table is now viewer_sessions (was
// mieter_sessions) and the FK column is viewer_mac (was mock_mac).
// The vocabulary swap matches the broader Mock -> Viewer rename
// across the platform; routing semantics are unchanged (one
// viewer = one device).
//
// Session ids are 32 random bytes encoded base64url-without-
// padding (43 characters). They live in viewer_sessions keyed by
// viewer_mac; RevokeAllForViewer wipes every active session for
// that viewer.
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

// Service manages viewer-session lifecycle against the
// viewer_sessions table.
type Service struct {
	db  *db.DB
	now func() time.Time
}

// Option mutates a Service during construction. Used for
// dependency injection in tests; production code passes no
// options.
type Option func(*Service)

// WithClock replaces the default time.Now source. Tests inject
// a closure they can advance to exercise expiry and rolling
// renewal paths.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// New constructs a Service. With no options it uses time.Now.
func New(d *db.DB, opts ...Option) *Service {
	s := &Service{db: d, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Create starts a new session for viewerMAC and returns the
// session id. expires_at is set to now + DefaultIdleTimeout.
func (s *Service) Create(ctx context.Context, viewerMAC string, meta Meta) (string, error) {
	if viewerMAC == "" {
		return "", errors.New("session: viewerMAC must not be empty")
	}
	sid, err := newSessionID()
	if err != nil {
		return "", fmt.Errorf("session: generate id: %w", err)
	}
	now := s.now()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO viewer_sessions
		   (session_id, viewer_mac, created_at, last_seen, expires_at, user_agent, ip)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sid, viewerMAC,
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
// other. Returns the viewer-MAC the session belongs to.
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
		viewerMAC string
		expiresAt int64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT viewer_mac, expires_at FROM viewer_sessions WHERE session_id = ?`,
		sessionID,
	).Scan(&viewerMAC, &expiresAt)
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
		`UPDATE viewer_sessions SET last_seen = ?, expires_at = ? WHERE session_id = ?`,
		now.UnixMilli(), now.Add(DefaultIdleTimeout).UnixMilli(), sessionID,
	); err != nil {
		return "", fmt.Errorf("session: update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("session: commit: %w", err)
	}
	return viewerMAC, nil
}

// Revoke deletes a single session. Missing sessions are not an
// error: revoke is idempotent.
func (s *Service) Revoke(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM viewer_sessions WHERE session_id = ?`, sessionID)
	if err != nil {
		return fmt.Errorf("session: delete: %w", err)
	}
	return nil
}

// RevokeAllForViewer deletes every session bound to viewerMAC
// and returns the number of rows removed. Useful when the admin
// resets a viewer's password (S13-02-FIX4-a) or removes the
// viewer (FK cascade handles the same outcome, but explicit
// feedback is nicer for the admin UI).
func (s *Service) RevokeAllForViewer(ctx context.Context, viewerMAC string) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM viewer_sessions WHERE viewer_mac = ?`, viewerMAC)
	if err != nil {
		return 0, fmt.Errorf("session: delete by viewer: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("session: rows affected: %w", err)
	}
	return int(n), nil
}

// CleanupExpired removes every session whose expires_at is in the
// past and returns the count. Intended to be called periodically
// by a future background job; in saison 13 it is implemented but
// not yet wired up.
func (s *Service) CleanupExpired(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM viewer_sessions WHERE expires_at < ?`, s.now().UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("session: cleanup: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("session: rows affected: %w", err)
	}
	return int(n), nil
}

// ActiveCount returns the number of viewer_sessions whose
// expires_at is still in the future. Cheap aggregate for the
// admin dashboard.
func (s *Service) ActiveCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM viewer_sessions WHERE expires_at > ?`,
		s.now().UnixMilli(),
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("session: active count: %w", err)
	}
	return n, nil
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
