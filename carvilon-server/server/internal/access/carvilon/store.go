// Package carvilon implementiert access.NativeUserStore gegen die
// carvilon_users-Tabelle (Migration 034) - CARVILONs eigene
// Benutzer als persistente, UA-unabhaengige Identitaeten.
//
// Es ist der einzige SQL-Schreiber fuer carvilon_users. Die IDs
// sind stabile UUID-v4-Strings; sie bleiben ueber Umbenennung,
// Aktiv-Toggle und Neustart hinweg konstant (Muster: die anderen
// Stores in internal/*store).
package carvilon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"carvilon.local/server/internal/access"
)

// Store ist das SQL-Gateway fuer CARVILONs eigene Benutzer.
type Store struct {
	db    *sql.DB
	now   func() time.Time
	newID func() string
}

// Option mutiert einen Store bei der Konstruktion.
type Option func(*Store)

// WithClock injiziert eine Test-Uhr.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// WithIDFunc injiziert einen deterministischen ID-Generator (Tests).
func WithIDFunc(fn func() string) Option {
	return func(s *Store) { s.newID = fn }
}

// New konstruiert einen Store. Standard-Uhr ist time.Now, Standard-
// ID-Generator ist uuid.NewString.
func New(db *sql.DB, opts ...Option) *Store {
	s := &Store{db: db, now: time.Now, newID: uuid.NewString}
	for _, o := range opts {
		o(s)
	}
	return s
}

// compile-time check: Store erfuellt das Adapter-Interface.
var _ access.NativeUserStore = (*Store)(nil)

// Create legt einen neuen Benutzer mit frischer UUID an. Aktiv per
// Default. Ein leerer Anzeigename wird abgelehnt.
func (s *Store) Create(ctx context.Context, params access.CreateNativeUserParams) (access.NativeUser, error) {
	name := strings.TrimSpace(params.DisplayName)
	if name == "" {
		return access.NativeUser{}, errors.New("carvilon: display name required")
	}
	now := s.now().UnixMilli()
	u := access.NativeUser{
		ID:          s.newID(),
		DisplayName: name,
		Active:      true,
		UAUserID:    strings.TrimSpace(params.UAUserID),
		CreatedAt:   time.UnixMilli(now),
		UpdatedAt:   time.UnixMilli(now),
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO carvilon_users (id, display_name, active, ua_user_id, created_at, updated_at)
		 VALUES (?, ?, 1, ?, ?, ?)`,
		u.ID, u.DisplayName, nullString(u.UAUserID), now, now,
	)
	if err != nil {
		return access.NativeUser{}, fmt.Errorf("carvilon: insert: %w", err)
	}
	return u, nil
}

// Get liefert einen Benutzer per ID oder access.ErrNotFound.
func (s *Store) Get(ctx context.Context, id string) (access.NativeUser, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, display_name, active, ua_user_id, created_at, updated_at
		   FROM carvilon_users WHERE id = ?`, id)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return access.NativeUser{}, access.ErrNotFound
	}
	if err != nil {
		return access.NativeUser{}, fmt.Errorf("carvilon: get: %w", err)
	}
	return u, nil
}

// List liefert die gefilterten Benutzer, sortiert nach Anzeigename
// (case-insensitive), aktive vor inaktiven. Kein Paging: der
// Personalbestand ist klein.
func (s *Store) List(ctx context.Context, params access.NativeListParams) ([]access.NativeUser, error) {
	query := `SELECT id, display_name, active, ua_user_id, created_at, updated_at
	            FROM carvilon_users`
	var where []string
	var args []any
	if params.Active != nil {
		where = append(where, "active = ?")
		if *params.Active {
			args = append(args, 1)
		} else {
			args = append(args, 0)
		}
	}
	if q := strings.TrimSpace(params.Query); q != "" {
		where = append(where, "LOWER(display_name) LIKE ?")
		args = append(args, "%"+strings.ToLower(q)+"%")
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	// active DESC: aktive Benutzer zuerst; danach alphabetisch.
	query += " ORDER BY active DESC, LOWER(display_name) ASC, id ASC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("carvilon: list: %w", err)
	}
	defer rows.Close()

	out := make([]access.NativeUser, 0)
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("carvilon: scan: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("carvilon: list rows: %w", err)
	}
	return out, nil
}

// Update schreibt den Anzeigenamen. UUID und Aktiv-Zustand bleiben
// unberuehrt. Leerer Name wird abgelehnt.
func (s *Store) Update(ctx context.Context, id string, params access.UpdateNativeUserParams) (access.NativeUser, error) {
	name := strings.TrimSpace(params.DisplayName)
	if name == "" {
		return access.NativeUser{}, errors.New("carvilon: display name required")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE carvilon_users SET display_name = ?, updated_at = ? WHERE id = ?`,
		name, s.now().UnixMilli(), id)
	if err != nil {
		return access.NativeUser{}, fmt.Errorf("carvilon: update: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return access.NativeUser{}, access.ErrNotFound
	}
	return s.Get(ctx, id)
}

// SetActive schaltet den Benutzer aktiv/inaktiv.
func (s *Store) SetActive(ctx context.Context, id string, active bool) error {
	val := 0
	if active {
		val = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE carvilon_users SET active = ?, updated_at = ? WHERE id = ?`,
		val, s.now().UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("carvilon: set active: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return access.ErrNotFound
	}
	return nil
}

// SetUALink heftet die optionale UA-Identitaet an den Benutzer oder
// loest sie (leerer uaUserID -> NULL). Die UA-Kopplung ist nur ein
// Attribut am CARVILON-Benutzer, kein eigener Eintrag. Ein bereits an
// einen anderen Benutzer vergebenes UA-Profil liefert
// access.ErrUALinkTaken (partieller UNIQUE-Index, faengt auch Races).
func (s *Store) SetUALink(ctx context.Context, id string, uaUserID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE carvilon_users SET ua_user_id = ?, updated_at = ? WHERE id = ?`,
		nullString(strings.TrimSpace(uaUserID)), s.now().UnixMilli(), id)
	if err != nil {
		if isUniqueViolation(err) {
			return access.ErrUALinkTaken
		}
		return fmt.Errorf("carvilon: set ua link: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return access.ErrNotFound
	}
	return nil
}

// Delete entfernt den Benutzer endgueltig.
func (s *Store) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM carvilon_users WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("carvilon: delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return access.ErrNotFound
	}
	return nil
}

// rowScanner deckt *sql.Row und *sql.Rows ab.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanUser(sc rowScanner) (access.NativeUser, error) {
	var (
		u         access.NativeUser
		active    int64
		uaUserID  sql.NullString
		createdMs int64
		updatedMs int64
	)
	if err := sc.Scan(&u.ID, &u.DisplayName, &active, &uaUserID, &createdMs, &updatedMs); err != nil {
		return access.NativeUser{}, err
	}
	u.Active = active != 0
	u.UAUserID = uaUserID.String
	u.CreatedAt = time.UnixMilli(createdMs)
	u.UpdatedAt = time.UnixMilli(updatedMs)
	return u, nil
}

// nullString mappt "" auf NULL, sonst auf den Wert. So bleibt
// ua_user_id sauber NULL statt Leerstring, wenn keine UA-Kopplung
// gesetzt ist.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// isUniqueViolation erkennt einen UNIQUE-Constraint-Bruch aus modernc/
// sqlite ueber die Fehlermeldung (gleiche Strategie wie telegramstore).
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToUpper(err.Error()), "UNIQUE")
}
