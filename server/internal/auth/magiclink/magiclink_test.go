package magiclink

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"unifix.local/server/internal/db"
)

const testMockMAC = "0c:ea:14:42:42:42"

func newTestService(t *testing.T) *Service {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	seedMock(t, d, testMockMAC)
	return New(d)
}

// seedMock inserts the minimal mock_viewers row needed for the
// magic_link_tokens.mock_mac FK to validate. Saison-12-06 made
// the FK hard, so every magic-link test needs a real mock first.
func seedMock(t *testing.T, d *db.DB, mac string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := d.Exec(
		`INSERT INTO mock_viewers (mac, name, service_port, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		mac, "Test "+mac, 9000, now, now,
	); err != nil {
		t.Fatalf("seed mock: %v", err)
	}
}

func isBase64URL(r rune) bool {
	return (r >= 'A' && r <= 'Z') ||
		(r >= 'a' && r <= 'z') ||
		(r >= '0' && r <= '9') ||
		r == '-' || r == '_'
}

func TestCreate_ReturnsValidToken(t *testing.T) {
	s := newTestService(t)
	token, err := s.Create(context.Background(), testMockMAC)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := len(token); got != 43 {
		t.Errorf("token length = %d, want 43", got)
	}
	for i, r := range token {
		if !isBase64URL(r) {
			t.Errorf("token[%d] = %q is not a base64url char", i, r)
			break
		}
	}
}

func TestCreate_PersistsToDB(t *testing.T) {
	s := newTestService(t)
	token, err := s.Create(context.Background(), testMockMAC)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var (
		mockMAC   string
		createdAt int64
		expiresAt int64
	)
	err = s.db.QueryRow(
		`SELECT mock_mac, created_at, expires_at FROM magic_link_tokens WHERE token = ?`,
		token,
	).Scan(&mockMAC, &createdAt, &expiresAt)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if mockMAC != testMockMAC {
		t.Errorf("mock_mac = %q, want %q", mockMAC, testMockMAC)
	}
	if expiresAt <= createdAt {
		t.Errorf("expires_at (%d) is not after created_at (%d)", expiresAt, createdAt)
	}
}

func TestCreate_EmptyMACRejected(t *testing.T) {
	s := newTestService(t)
	if _, err := s.Create(context.Background(), ""); err == nil {
		t.Fatal("Create with empty mockMAC returned nil error")
	}
}

func TestCreateWithTTL_ZeroRejected(t *testing.T) {
	s := newTestService(t)
	if _, err := s.CreateWithTTL(context.Background(), testMockMAC, 0); err == nil {
		t.Fatal("CreateWithTTL with zero ttl returned nil error")
	}
}

func TestCreate_FKEnforcesExistingMock(t *testing.T) {
	s := newTestService(t)
	_, err := s.Create(context.Background(), "0c:ea:14:99:99:99")
	if err == nil {
		t.Fatal("Create with unknown mock_mac succeeded; FK not enforced")
	}
}

func TestConsume_HappyPath(t *testing.T) {
	s := newTestService(t)
	token, err := s.Create(context.Background(), testMockMAC)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := s.Consume(context.Background(), token)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if got != testMockMAC {
		t.Errorf("Consume = %q, want %q", got, testMockMAC)
	}
	var consumedAt int64
	if err := s.db.QueryRow(
		`SELECT consumed_at FROM magic_link_tokens WHERE token = ?`, token,
	).Scan(&consumedAt); err != nil {
		t.Fatalf("query consumed_at: %v", err)
	}
	if consumedAt == 0 {
		t.Error("consumed_at not stamped after Consume")
	}
}

func TestConsume_NotFound(t *testing.T) {
	s := newTestService(t)
	_, err := s.Consume(context.Background(), "no-such-token")
	if !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("err = %v, want ErrTokenNotFound", err)
	}
}

func TestConsume_EmptyTokenNotFound(t *testing.T) {
	s := newTestService(t)
	_, err := s.Consume(context.Background(), "")
	if !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("err = %v, want ErrTokenNotFound", err)
	}
}

func TestConsume_Expired(t *testing.T) {
	s := newTestService(t)
	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }
	token, err := s.Create(context.Background(), testMockMAC)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	s.now = func() time.Time { return base.Add(DefaultTTL + time.Second) }
	_, err = s.Consume(context.Background(), token)
	if !errors.Is(err, ErrTokenExpired) {
		t.Errorf("err = %v, want ErrTokenExpired", err)
	}
}

func TestConsume_AlreadyConsumed(t *testing.T) {
	s := newTestService(t)
	token, err := s.Create(context.Background(), testMockMAC)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Consume(context.Background(), token); err != nil {
		t.Fatalf("first Consume: %v", err)
	}
	_, err = s.Consume(context.Background(), token)
	if !errors.Is(err, ErrTokenConsumed) {
		t.Errorf("second Consume = %v, want ErrTokenConsumed", err)
	}
}
