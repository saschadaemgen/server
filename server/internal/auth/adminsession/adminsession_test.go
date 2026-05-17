package adminsession

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"carvilon.local/server/internal/db"
)

const testAdmin = "sascha"

func newTestService(t *testing.T) *Service {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	seedAdmin(t, d, testAdmin)
	return New(d)
}

func seedAdmin(t *testing.T, d *db.DB, username string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := d.Exec(
		`INSERT INTO admin_users (username, password_hash, created_at, updated_at)
		 VALUES (?, ?, ?, ?)`,
		username, "hash-placeholder", now, now,
	); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
}

func TestCreate_HappyPath(t *testing.T) {
	s := newTestService(t)
	sid, err := s.Create(context.Background(), testAdmin, Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(sid) != 43 {
		t.Errorf("session id length = %d, want 43", len(sid))
	}
}

func TestCreate_EmptyUsernameRejected(t *testing.T) {
	s := newTestService(t)
	if _, err := s.Create(context.Background(), "", Meta{}); err == nil {
		t.Fatal("Create with empty username returned nil error")
	}
}

func TestCreate_FKEnforcesExistingAdmin(t *testing.T) {
	s := newTestService(t)
	_, err := s.Create(context.Background(), "ghost-admin", Meta{})
	if err == nil {
		t.Fatal("Create with unknown admin_username succeeded; FK not enforced")
	}
}

func TestValidate_HappyPath_RollingRenewal(t *testing.T) {
	s := newTestService(t)
	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }
	sid, err := s.Create(context.Background(), testAdmin, Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	later := base.Add(time.Hour)
	s.now = func() time.Time { return later }
	user, err := s.Validate(context.Background(), sid)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if user != testAdmin {
		t.Errorf("Validate = %q, want %q", user, testAdmin)
	}
	var expiresAt int64
	if err := s.db.QueryRow(
		`SELECT expires_at FROM admin_sessions WHERE session_id = ?`, sid,
	).Scan(&expiresAt); err != nil {
		t.Fatalf("query: %v", err)
	}
	want := later.Add(DefaultIdleTimeout).UnixMilli()
	if expiresAt != want {
		t.Errorf("expires_at = %d, want %d", expiresAt, want)
	}
}

func TestValidate_NotFound(t *testing.T) {
	s := newTestService(t)
	_, err := s.Validate(context.Background(), "ghost-sid")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("err = %v, want ErrSessionNotFound", err)
	}
}

func TestValidate_Expired(t *testing.T) {
	s := newTestService(t)
	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }
	sid, err := s.Create(context.Background(), testAdmin, Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	s.now = func() time.Time { return base.Add(DefaultIdleTimeout + time.Second) }
	_, err = s.Validate(context.Background(), sid)
	if !errors.Is(err, ErrSessionExpired) {
		t.Errorf("err = %v, want ErrSessionExpired", err)
	}
}

func TestRevoke_RemovesAndIsIdempotent(t *testing.T) {
	s := newTestService(t)
	sid, err := s.Create(context.Background(), testAdmin, Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Revoke(context.Background(), sid); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := s.Validate(context.Background(), sid); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("Validate after Revoke = %v, want ErrSessionNotFound", err)
	}
	if err := s.Revoke(context.Background(), "no-such-session"); err != nil {
		t.Errorf("Revoke missing = %v, want nil", err)
	}
}

func TestCleanupExpired(t *testing.T) {
	s := newTestService(t)
	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }
	if _, err := s.Create(context.Background(), testAdmin, Meta{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Create(context.Background(), testAdmin, Meta{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	s.now = func() time.Time { return base.Add(DefaultIdleTimeout + time.Second) }
	if _, err := s.Create(context.Background(), testAdmin, Meta{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	n, err := s.CleanupExpired(context.Background())
	if err != nil {
		t.Fatalf("CleanupExpired: %v", err)
	}
	if n != 2 {
		t.Errorf("CleanupExpired = %d, want 2", n)
	}
}
