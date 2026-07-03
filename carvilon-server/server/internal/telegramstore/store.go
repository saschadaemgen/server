// Package telegramstore is the persistence layer for the Telegram
// bot's chat allowlist and its pending (awaiting-approval) chats
// (migration 031). It is the single SQL writer for the
// telegram_allowed_chats and telegram_pending_chats tables.
//
// The allowlist is the bot's default-deny gate: only chat IDs with a
// row here may trigger commands and receive messages. Unknown chats
// that write to the bot land in the pending table so the admin can
// approve them in-product (no third-party chat-id tool), mirroring
// the ESP adoption flow. The bot loads a full allowlist snapshot
// (LoadAllowlist) at start and after every admin change; per-message
// checks run against that in-memory snapshot, never a live query.
package telegramstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrChatExists is returned when a chat ID is already allowlisted.
	ErrChatExists = errors.New("telegramstore: chat already allowed")
	// ErrChatNotFound is returned when a chat ID has no row.
	ErrChatNotFound = errors.New("telegramstore: chat not found")
)

// pendingCap bounds the non-rejected pending table: chat IDs are cheap
// to mint (every new group is a fresh one), so an attacker who learns
// the bot username must not be able to grow the table without bound.
// The newest rows win; older waiting rows are evicted.
const pendingCap = 100

// Store is the SQL gateway for the Telegram chat allowlist.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Option mutates a Store during construction.
type Option func(*Store)

// WithClock injects a test clock.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// New constructs a Store.
func New(db *sql.DB, opts ...Option) *Store {
	s := &Store{db: db, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// AllowedChat is a read view of one allowlist row.
type AllowedChat struct {
	ChatID    int64
	Label     string
	CreatedAt int64
}

// PendingChat is a read view of one awaiting-approval row. Username
// and FirstName are display metadata taken from the chat's messages
// (attacker-controlled free text - render through html/template only).
type PendingChat struct {
	ChatID    int64
	Username  string
	FirstName string
	FirstSeen int64
	LastSeen  int64
	Rejected  bool
}

// AddAllowed inserts a chat into the allowlist (manual admin add or
// Approve). Returns ErrChatExists on a duplicate chat ID.
func (s *Store) AddAllowed(ctx context.Context, chatID int64, label string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO telegram_allowed_chats (chat_id, label, created_at) VALUES (?, ?, ?)`,
		chatID, strings.TrimSpace(label), s.now().UnixMilli(),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrChatExists
		}
		return fmt.Errorf("telegramstore: insert allowed: %w", err)
	}
	return nil
}

// RemoveAllowed deletes a chat from the allowlist.
func (s *Store) RemoveAllowed(ctx context.Context, chatID int64) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM telegram_allowed_chats WHERE chat_id = ?`, chatID)
	if err != nil {
		return fmt.Errorf("telegramstore: delete allowed: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrChatNotFound
	}
	return nil
}

// ListAllowed returns the allowlist ordered by label, then chat ID.
func (s *Store) ListAllowed(ctx context.Context) ([]AllowedChat, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT chat_id, label, created_at FROM telegram_allowed_chats
		  ORDER BY label, chat_id`)
	if err != nil {
		return nil, fmt.Errorf("telegramstore: list allowed: %w", err)
	}
	defer rows.Close()
	var out []AllowedChat
	for rows.Next() {
		var c AllowedChat
		if err := rows.Scan(&c.ChatID, &c.Label, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("telegramstore: scan allowed: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// LoadAllowlist reads the full allowlist into the in-memory snapshot
// the bot checks per message (chat ID -> label). Called at start and
// after every admin change; never queried per message.
func (s *Store) LoadAllowlist(ctx context.Context) (map[int64]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT chat_id, label FROM telegram_allowed_chats`)
	if err != nil {
		return nil, fmt.Errorf("telegramstore: load allowlist: %w", err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var label string
		if err := rows.Scan(&id, &label); err != nil {
			return nil, fmt.Errorf("telegramstore: scan allowlist: %w", err)
		}
		out[id] = label
	}
	return out, rows.Err()
}

// UpsertPending records (or refreshes) an unknown chat that wrote to
// the bot. A previous rejection is preserved - a rejected chat that
// keeps writing must not resurface as "waiting". The waiting set is
// capped at pendingCap rows (newest win); rejected rows are bounded by
// admin actions and left alone.
func (s *Store) UpsertPending(ctx context.Context, chatID int64, username, firstName string) error {
	now := s.now().UnixMilli()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO telegram_pending_chats (chat_id, username, first_name, first_seen, last_seen)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(chat_id) DO UPDATE SET
		   username = excluded.username,
		   first_name = excluded.first_name,
		   last_seen = excluded.last_seen`,
		chatID, nullable(username), nullable(firstName), now, now,
	)
	if err != nil {
		return fmt.Errorf("telegramstore: upsert pending: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`DELETE FROM telegram_pending_chats
		  WHERE rejected_at IS NULL
		    AND chat_id NOT IN (
		      SELECT chat_id FROM telegram_pending_chats
		       WHERE rejected_at IS NULL
		       ORDER BY last_seen DESC LIMIT ?)`, pendingCap)
	if err != nil {
		return fmt.Errorf("telegramstore: cap pending: %w", err)
	}
	return nil
}

// ListPending returns the pending chats, newest activity first,
// including rejected ones (the caller splits waiting vs rejected).
func (s *Store) ListPending(ctx context.Context) ([]PendingChat, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT chat_id, COALESCE(username,''), COALESCE(first_name,''),
		        first_seen, last_seen, rejected_at IS NOT NULL
		   FROM telegram_pending_chats ORDER BY last_seen DESC`)
	if err != nil {
		return nil, fmt.Errorf("telegramstore: list pending: %w", err)
	}
	defer rows.Close()
	var out []PendingChat
	for rows.Next() {
		var c PendingChat
		if err := rows.Scan(&c.ChatID, &c.Username, &c.FirstName, &c.FirstSeen, &c.LastSeen, &c.Rejected); err != nil {
			return nil, fmt.Errorf("telegramstore: scan pending: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Approve moves a pending chat onto the allowlist (one transaction:
// insert allowed + delete pending). ErrChatNotFound when the chat is
// not pending. A chat that is ALREADY allowed (added manually while
// also pending) counts as approved: the pending row is cleared and
// the existing allowlist entry - its label included - stays; rolling
// back instead would leave the card stuck as "wartend" forever.
func (s *Store) Approve(ctx context.Context, chatID int64, label string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("telegramstore: begin approve: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`DELETE FROM telegram_pending_chats WHERE chat_id = ?`, chatID)
	if err != nil {
		return fmt.Errorf("telegramstore: approve delete pending: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrChatNotFound
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO telegram_allowed_chats (chat_id, label, created_at) VALUES (?, ?, ?)`,
		chatID, strings.TrimSpace(label), s.now().UnixMilli())
	if err != nil && !isUniqueViolation(err) {
		return fmt.Errorf("telegramstore: approve insert allowed: %w", err)
	}
	return tx.Commit()
}

// Reject marks a pending chat as rejected; the row stays so the chat's
// next message does not resurface it as waiting.
func (s *Store) Reject(ctx context.Context, chatID int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE telegram_pending_chats SET rejected_at = ? WHERE chat_id = ?`,
		s.now().UnixMilli(), chatID)
	if err != nil {
		return fmt.Errorf("telegramstore: reject: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrChatNotFound
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// isUniqueViolation classifies modernc sqlite errors by message
// substring (the driver does not export typed codes here).
func isUniqueViolation(err error) bool {
	return strings.Contains(strings.ToUpper(err.Error()), "UNIQUE")
}
