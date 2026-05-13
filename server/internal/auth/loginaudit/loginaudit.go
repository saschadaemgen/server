// Package loginaudit schreibt jeden Web-Viewer- und Admin-Login-
// Versuch (success / fail / locked / unlocked) in die login_audit-
// Tabelle (Migration 006). Das Admin-Dashboard rendert die letzten
// Eintraege; Settings-Seite zeigt eine groessere Liste plus Filter.
//
// Ein Background-Job (Cleanup) loescht Eintraege die aelter als
// retentionDays sind. In Saison 13-02-FIX4-a wird Cleanup einmal
// pro Stunde aus dem Server-Boot-Loop angetriggert.
package loginaudit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"unifix.local/server/internal/db"
)

// DefaultRetention ist die Aufbewahrungsdauer fuer Audit-Eintraege.
const DefaultRetention = 90 * 24 * time.Hour

// Outcome listet die zulaessigen outcome-Werte.
type Outcome string

const (
	OutcomeSuccess  Outcome = "success"
	OutcomeFail     Outcome = "fail"
	OutcomeLocked   Outcome = "locked"
	OutcomeUnlocked Outcome = "unlocked"
)

// Realm grenzt Web-Viewer- gegen Admin-Eintraege ab.
type Realm string

const (
	RealmViewer Realm = "viewer"
	RealmAdmin  Realm = "admin"
)

// Entry ist eine Zeile aus login_audit.
type Entry struct {
	ID         int64
	Timestamp  time.Time
	Realm      Realm
	Username   string
	ViewerMAC  string
	IP         string
	UserAgent  string
	Outcome    Outcome
}

// Service kapselt die DB-Operationen.
type Service struct {
	db  *db.DB
	now func() time.Time
}

// New baut einen Service mit time.Now als Clock-Quelle.
func New(d *db.DB) *Service {
	return NewWithClock(d, time.Now)
}

// NewWithClock injiziert eine Test-Clock.
func NewWithClock(d *db.DB, now func() time.Time) *Service {
	return &Service{db: d, now: now}
}

// Insert schreibt einen neuen Eintrag. Fehler werden lediglich
// zurueckgegeben; Login-Pfade loggen die Fehler aber blocken nicht
// weil der Login-Outcome wichtiger ist als der Audit-Eintrag.
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

// Recent liefert die letzten N Eintraege (newest first), optional
// gefiltert auf einen Realm. limit <= 0 -> 50.
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

// Cleanup loescht Eintraege aelter als retention.
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
		e          Entry
		ts         int64
		realm      string
		username   sql.NullString
		viewerMAC  sql.NullString
		ip         sql.NullString
		userAgent  sql.NullString
		outcome    string
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
