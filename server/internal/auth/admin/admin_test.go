package admin

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"unifix.local/server/internal/db"
)

func newTestService(t *testing.T) (*Service, *db.DB) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return New(d), d
}

func TestExists_EmptyDB(t *testing.T) {
	s, _ := newTestService(t)
	ok, err := s.Exists(context.Background())
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if ok {
		t.Error("Exists on empty DB = true, want false")
	}
}

func TestSetPassword_CreatesUser(t *testing.T) {
	s, _ := newTestService(t)
	if err := s.SetPassword(context.Background(), "admin", "supersafe1!"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	ok, _ := s.Exists(context.Background())
	if !ok {
		t.Error("Exists after SetPassword = false, want true")
	}
}

func TestSetPassword_Upsert(t *testing.T) {
	s, _ := newTestService(t)
	if err := s.SetPassword(context.Background(), "admin", "password1234"); err != nil {
		t.Fatalf("first SetPassword: %v", err)
	}
	if err := s.SetPassword(context.Background(), "admin", "newpassword5678"); err != nil {
		t.Fatalf("second SetPassword: %v", err)
	}
	// old password no longer works
	err := s.Login(context.Background(), "admin", "password1234")
	if !errors.Is(err, ErrInvalidPassword) {
		t.Errorf("Login with old password = %v, want ErrInvalidPassword", err)
	}
	// new password works
	if err := s.Login(context.Background(), "admin", "newpassword5678"); err != nil {
		t.Errorf("Login with new password = %v, want nil", err)
	}
}

func TestSetPassword_RejectsEmptyUsername(t *testing.T) {
	s, _ := newTestService(t)
	if err := s.SetPassword(context.Background(), "", "supersafe1!"); err == nil {
		t.Fatal("SetPassword with empty username returned nil")
	}
}

func TestSetPassword_RejectsShortPassword(t *testing.T) {
	s, _ := newTestService(t)
	if err := s.SetPassword(context.Background(), "admin", "short"); err == nil {
		t.Fatal("SetPassword with short password returned nil")
	}
}

func TestLogin_HappyPath_BumpsLastLogin(t *testing.T) {
	s, d := newTestService(t)
	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }
	if err := s.SetPassword(context.Background(), "admin", "supersafe1!"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	later := base.Add(time.Hour)
	s.now = func() time.Time { return later }
	if err := s.Login(context.Background(), "admin", "supersafe1!"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	var lastLoginAt int64
	if err := d.QueryRow(
		`SELECT last_login_at FROM admin_users WHERE username = ?`, "admin",
	).Scan(&lastLoginAt); err != nil {
		t.Fatalf("query last_login_at: %v", err)
	}
	if lastLoginAt != later.UnixMilli() {
		t.Errorf("last_login_at = %d, want %d", lastLoginAt, later.UnixMilli())
	}
}

func TestLogin_WrongPassword(t *testing.T) {
	s, _ := newTestService(t)
	if err := s.SetPassword(context.Background(), "admin", "supersafe1!"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	err := s.Login(context.Background(), "admin", "wrong-password")
	if !errors.Is(err, ErrInvalidPassword) {
		t.Errorf("Login = %v, want ErrInvalidPassword", err)
	}
}

func TestLogin_UnknownUser(t *testing.T) {
	s, _ := newTestService(t)
	err := s.Login(context.Background(), "ghost", "anything-12345")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Login unknown = %v, want ErrNotFound", err)
	}
}
