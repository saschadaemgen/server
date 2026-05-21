// Package loginaudit writes every web-viewer and admin login
// attempt (success / fail / locked / unlocked) into the
// login_audit table (Migration 006). The admin dashboard renders
// the most recent entries; the settings page shows a longer list
// with filters.
//
// A background job (Cleanup) drops entries older than the
// configured retention. The server boot loop runs Cleanup once
// per hour.
package loginaudit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"carvilon.local/server/internal/db"
)

// DefaultRetention is how long audit entries are kept by default.
const DefaultRetention = 90 * 24 * time.Hour

// Outcome enumerates the valid outcome values.
type Outcome string

const (
	OutcomeSuccess  Outcome = "success"
	OutcomeFail     Outcome = "fail"
	OutcomeLocked   Outcome = "locked"
	OutcomeUnlocked Outcome = "unlocked"
)

// Realm separates web-viewer entries from admin entries.
type Realm string

const (
	RealmViewer Realm = "viewer"
	RealmAdmin  Realm = "admin"
)

// Entry is one row from login_audit.
type Entry struct {
	ID        int64
	Timestamp time.Time
	Realm     Realm
	Username  string
	ViewerMAC string
	IP        string
	UserAgent string
	Outcome   Outcome
}

// Service wraps the DB operations.
type Service struct {
	db  *db.DB
	now func() time.Time
}

// New builds a Service with time.Now as the clock source.
func New(d *db.DB) *Service {
	return NewWithClock(d, time.Now)
}

// NewWithClock injects a test clock.
func NewWithClock(d *db.DB, now func() time.Time) *Service {
	return &Service{db: d, now: now}
}

// Insert writes a new entry. Errors are returned as-is; the login
// paths log them but do not block, because the login outcome
// matters more than the audit row.
func (s *Service) Insert(ctx context.Context, e Entry) error {
	if e.Outcome == "" {
		return errors.New("loginaudit: outcome required")
	}
	if e.Realm == "" {
		e.Realm = RealmViewer
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = s.now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO login_audit
		   (timestamp, realm, username, viewer_mac, ip, user_agent, outcome)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.Timestamp.UnixMilli(), string(e.Realm),
		nullable(e.Username), nullable(e.ViewerMAC),
		nullable(e.IP), nullable(e.UserAgent),
		string(e.Outcome),
	)
	if err != nil {
		return fmt.Errorf("loginaudit: insert: %w", err)
	}
	return nil
}

// Recent returns the last N entries (newest first), optionally
// filtered to a single Realm. limit <= 0 -> 50.
func (s *Service) Recent(ctx context.Context, realm Realm, limit int) ([]Entry, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT id, timestamp, realm, username, viewer_mac, ip, user_agent, outcome
	        FROM login_audit`
	args := []any{}
	if realm != "" {
		q += ` WHERE realm = ?`
		args = append(args, string(realm))
	}
	q += ` ORDER BY timestamp DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("loginaudit: query: %w", err)
	}
	defer rows.Close()
	out := make([]Entry, 0, limit)
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("loginaudit: rows: %w", err)
	}
	return out, nil
}

// Cleanup deletes entries older than retention.
func (s *Service) Cleanup(ctx context.Context, retention time.Duration) (int, error) {
	if retention <= 0 {
		retention = DefaultRetention
	}
	cutoff := s.now().Add(-retention).UnixMilli()
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM login_audit WHERE timestamp < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("loginaudit: cleanup: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("loginaudit: rows affected: %w", err)
	}
	return int(n), nil
}

func scan(rows *sql.Rows) (Entry, error) {
	var (
		e         Entry
		ts        int64
		realm     string
		username  sql.NullString
		viewerMAC sql.NullString
		ip        sql.NullString
		userAgent sql.NullString
		outcome   string
	)
	if err := rows.Scan(&e.ID, &ts, &realm, &username, &viewerMAC, &ip, &userAgent, &outcome); err != nil {
		return Entry{}, fmt.Errorf("loginaudit: scan: %w", err)
	}
	e.Timestamp = time.UnixMilli(ts)
	e.Realm = Realm(realm)
	if username.Valid {
		e.Username = username.String
	}
	if viewerMAC.Valid {
		e.ViewerMAC = viewerMAC.String
	}
	if ip.Valid {
		e.IP = ip.String
	}
	if userAgent.Valid {
		e.UserAgent = userAgent.String
	}
	e.Outcome = Outcome(outcome)
	return e, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
