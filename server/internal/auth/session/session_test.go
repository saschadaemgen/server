package session

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"unifix.local/server/internal/db"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return New(d)
}

func TestCreate_ReturnsValidSessionID(t *testing.T) {
	s := newTestService(t)
	sid, err := s.Create(context.Background(), "ua-user-1", Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(sid) != 43 {
		t.Errorf("session id length = %d, want 43", len(sid))
	}
}

func TestCreate_PersistsMetadata(t *testing.T) {
	s := newTestService(t)
	meta := Meta{UserAgent: "Test/1.0", IP: "192.168.1.42"}
	sid, err := s.Create(context.Background(), "ua-user-1", meta)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var (
		uaUserID  string
		userAgent string
		ip        string
	)
	err = s.db.QueryRow(
		`SELECT ua_user_id, user_agent, ip FROM sessions WHERE session_id = ?`, sid,
	).Scan(&uaUserID, &userAgent, &ip)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if uaUserID != "ua-user-1" {
		t.Errorf("ua_user_id = %q, want %q", uaUserID, "ua-user-1")
	}
	if userAgent != meta.UserAgent {
		t.Errorf("user_agent = %q, want %q", userAgent, meta.UserAgent)
	}
	if ip != meta.IP {
		t.Errorf("ip = %q, want %q", ip, meta.IP)
	}
}

func TestCreate_EmptyUserRejected(t *testing.T) {
	s := newTestService(t)
	if _, err := s.Create(context.Background(), "", Meta{}); err == nil {
		t.Fatal("Create with empty uaUserID returned nil error")
	}
}

func TestValidate_HappyPath_UpdatesLastSeen(t *testing.T) {
	s := newTestService(t)
	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }
	sid, err := s.Create(context.Background(), "ua-user-1", Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	later := base.Add(time.Hour)
	s.now = func() time.Time { return later }
	got, err := s.Validate(context.Background(), sid)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got != "ua-user-1" {
		t.Errorf("Validate = %q, want %q", got, "ua-user-1")
	}
	var lastSeen int64
	if err := s.db.QueryRow(
		`SELECT last_seen FROM sessions WHERE session_id = ?`, sid,
	).Scan(&lastSeen); err != nil {
		t.Fatalf("query last_seen: %v", err)
	}
	if lastSeen != later.UnixMilli() {
		t.Errorf("last_seen = %d, want %d", lastSeen, later.UnixMilli())
	}
}

func TestValidate_NotFound(t *testing.T) {
	s := newTestService(t)
	_, err := s.Validate(context.Background(), "no-such-session")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("err = %v, want ErrSessionNotFound", err)
	}
}

func TestValidate_EmptyIDNotFound(t *testing.T) {
	s := newTestService(t)
	_, err := s.Validate(context.Background(), "")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("err = %v, want ErrSessionNotFound", err)
	}
}

func TestValidate_Expired(t *testing.T) {
	s := newTestService(t)
	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }
	sid, err := s.Create(context.Background(), "ua-user-1", Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	s.now = func() time.Time { return base.Add(DefaultIdleTimeout + time.Second) }
	_, err = s.Validate(context.Background(), sid)
	if !errors.Is(err, ErrSessionExpired) {
		t.Errorf("err = %v, want ErrSessionExpired", err)
	}
}

func TestValidate_RollingRenewal(t *testing.T) {
	s := newTestService(t)
	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }
	sid, err := s.Create(context.Background(), "ua-user-1", Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	later := base.Add(24 * time.Hour)
	s.now = func() time.Time { return later }
	if _, err := s.Validate(context.Background(), sid); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	var expiresAt int64
	if err := s.db.QueryRow(
		`SELECT expires_at FROM sessions WHERE session_id = ?`, sid,
	).Scan(&expiresAt); err != nil {
		t.Fatalf("query expires_at: %v", err)
	}
	want := later.Add(DefaultIdleTimeout).UnixMilli()
	if expiresAt != want {
		t.Errorf("expires_at = %d, want %d (rolling renewal broken)", expiresAt, want)
	}
}

func TestRevoke_RemovesSession(t *testing.T) {
	s := newTestService(t)
	sid, err := s.Create(context.Background(), "ua-user-1", Meta{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Revoke(context.Background(), sid); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	var count int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sessions WHERE session_id = ?`, sid,
	).Scan(&count); err != nil {
		t.Fatalf("query count: %v", err)
	}
	if count != 0 {
		t.Errorf("row count after Revoke = %d, want 0", count)
	}
	if _, err := s.Validate(context.Background(), sid); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("Validate after Revoke = %v, want ErrSessionNotFound", err)
	}
}

func TestRevoke_IdempotentOnMissing(t *testing.T) {
	s := newTestService(t)
	if err := s.Revoke(context.Background(), "no-such-session"); err != nil {
		t.Errorf("Revoke missing = %v, want nil", err)
	}
}

func TestRevokeAll_ForUser(t *testing.T) {
	s := newTestService(t)
	for i := 0; i < 3; i++ {
		if _, err := s.Create(context.Background(), "ua-user-1", Meta{}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	// session for a different user must survive
	otherSID, err := s.Create(context.Background(), "ua-user-other", Meta{})
	if err != nil {
		t.Fatalf("Create other: %v", err)
	}
	n, err := s.RevokeAll(context.Background(), "ua-user-1")
	if err != nil {
		t.Fatalf("RevokeAll: %v", err)
	}
	if n != 3 {
		t.Errorf("RevokeAll = %d, want 3", n)
	}
	var remaining int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sessions WHERE ua_user_id = ?`, "ua-user-1",
	).Scan(&remaining); err != nil {
		t.Fatalf("query: %v", err)
	}
	if remaining != 0 {
		t.Errorf("ua-user-1 rows after RevokeAll = %d, want 0", remaining)
	}
	// untouched user still has session
	if _, err := s.Validate(context.Background(), otherSID); err != nil {
		t.Errorf("other-user session lost: %v", err)
	}
}

func TestCleanupExpired(t *testing.T) {
	s := newTestService(t)
	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }
	expired1, err := s.Create(context.Background(), "ua-user-1", Meta{})
	if err != nil {
		t.Fatalf("Create expired1: %v", err)
	}
	expired2, err := s.Create(context.Background(), "ua-user-1", Meta{})
	if err != nil {
		t.Fatalf("Create expired2: %v", err)
	}
	// jump past expiry, then create a session that is still valid
	s.now = func() time.Time { return base.Add(DefaultIdleTimeout + time.Second) }
	valid, err := s.Create(context.Background(), "ua-user-1", Meta{})
	if err != nil {
		t.Fatalf("Create valid: %v", err)
	}
	n, err := s.CleanupExpired(context.Background())
	if err != nil {
		t.Fatalf("CleanupExpired: %v", err)
	}
	if n != 2 {
		t.Errorf("CleanupExpired = %d, want 2", n)
	}
	for _, sid := range []string{expired1, expired2} {
		var count int
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM sessions WHERE session_id = ?`, sid,
		).Scan(&count); err != nil {
			t.Fatalf("query expired: %v", err)
		}
		if count != 0 {
			t.Errorf("expired session %s survived cleanup", sid[:8])
		}
	}
	var count int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sessions WHERE session_id = ?`, valid,
	).Scan(&count); err != nil {
		t.Fatalf("query valid: %v", err)
	}
	if count != 1 {
		t.Errorf("valid session removed by cleanup")
	}
}
