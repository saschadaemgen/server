package admin

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"unifix.local/server/internal/db"
)

const testPassword = "supersafe-1234!"
const testPasswordB = "newpassword-5678X"

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
	if err := s.SetPassword(context.Background(), "admin", testPassword); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	ok, _ := s.Exists(context.Background())
	if !ok {
		t.Error("Exists after SetPassword = false, want true")
	}
}

func TestSetPassword_Upsert(t *testing.T) {
	s, _ := newTestService(t)
	if err := s.SetPassword(context.Background(), "admin", testPassword); err != nil {
		t.Fatalf("first SetPassword: %v", err)
	}
	if err := s.SetPassword(context.Background(), "admin", testPasswordB); err != nil {
		t.Fatalf("second SetPassword: %v", err)
	}
	err := s.Login(context.Background(), "admin", testPassword)
	if !errors.Is(err, ErrInvalidPassword) {
		t.Errorf("Login with old password = %v, want ErrInvalidPassword", err)
	}
	if err := s.Login(context.Background(), "admin", testPasswordB); err != nil {
		t.Errorf("Login with new password = %v, want nil", err)
	}
}

func TestSetPassword_RejectsEmptyUsername(t *testing.T) {
	s, _ := newTestService(t)
	if err := s.SetPassword(context.Background(), "", testPassword); err == nil {
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
	if err := s.SetPassword(context.Background(), "admin", testPassword); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	later := base.Add(time.Hour)
	s.now = func() time.Time { return later }
	if err := s.Login(context.Background(), "admin", testPassword); err != nil {
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
	if err := s.SetPassword(context.Background(), "admin", testPassword); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	err := s.Login(context.Background(), "admin", "wrong-password-1234")
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

// TestLogin_BcryptRehash setzt einen bcrypt-Hash direkt in die DB
// (wie ein vor S13-02-FIX4-a angelegter Admin), pruft dass der
// erste Login funktioniert und der Hash danach Argon2id-Format
// hat.
func TestLogin_BcryptRehash(t *testing.T) {
	s, d := newTestService(t)
	now := time.Now().UnixMilli()
	// bcrypt hash of "legacy-password-1234" cost 4 fuer Test-Speed.
	hash, err := bcryptForTest("legacy-password-1234")
	if err != nil {
		t.Fatalf("bcryptForTest: %v", err)
	}
	if _, err := d.Exec(
		`INSERT INTO admin_users (username, password_hash, created_at, updated_at)
		 VALUES (?, ?, ?, ?)`, "admin", hash, now, now,
	); err != nil {
		t.Fatalf("insert legacy admin: %v", err)
	}
	if err := s.Login(context.Background(), "admin", "legacy-password-1234"); err != nil {
		t.Fatalf("Login with bcrypt hash: %v", err)
	}
	var newHash string
	if err := d.QueryRow(
		`SELECT password_hash FROM admin_users WHERE username = ?`, "admin",
	).Scan(&newHash); err != nil {
		t.Fatalf("query rehash: %v", err)
	}
	if newHash == hash {
		t.Error("hash unchanged after bcrypt login (rehash-on-login broken)")
	}
	if !looksLikeArgon2idForTest(newHash) {
		t.Errorf("rehashed value not Argon2id: %s", newHash)
	}
	if err := s.Login(context.Background(), "admin", "legacy-password-1234"); err != nil {
		t.Errorf("second login (now via Argon2id) failed: %v", err)
	}
}

func bcryptForTest(pw string) (string, error) {
	out, err := bcrypt.GenerateFromPassword([]byte(pw), 4)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func looksLikeArgon2idForTest(s string) bool {
	return len(s) > 10 && s[:10] == "$argon2id$"
}
