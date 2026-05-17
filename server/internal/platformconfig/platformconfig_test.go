package platformconfig

import (
	"context"
	"path/filepath"
	"testing"

	"carvilon.local/server/internal/db"
	"carvilon.local/server/internal/secrets"
)

func newTestService(t *testing.T) (*Service, *db.DB) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	secretsSvc, err := secrets.NewWithKey(key)
	if err != nil {
		t.Fatalf("secrets.NewWithKey: %v", err)
	}
	return New(d, secretsSvc), d
}

func TestGet_MissingReturnsEmptyString(t *testing.T) {
	s, _ := newTestService(t)
	got, err := s.Get(context.Background(), "unknown")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "" {
		t.Errorf("Get = %q, want empty", got)
	}
}

func TestSetAndGet_Plaintext(t *testing.T) {
	s, _ := newTestService(t)
	if err := s.Set(context.Background(), KeyUAAPIBaseURL, "https://example.com:12445"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get(context.Background(), KeyUAAPIBaseURL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "https://example.com:12445" {
		t.Errorf("Get = %q, want example.com URL", got)
	}
}

func TestSetSecretAndGetSecret(t *testing.T) {
	s, d := newTestService(t)
	if err := s.SetSecret(context.Background(), KeyUAAPIToken, "supersecret-token-value"); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	got, err := s.GetSecret(context.Background(), KeyUAAPIToken)
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if got != "supersecret-token-value" {
		t.Errorf("GetSecret = %q, want plaintext", got)
	}
	// raw DB row must NOT contain the plaintext
	var enc string
	if err := d.QueryRow(
		`SELECT value_encrypted FROM platform_config WHERE key = ?`, KeyUAAPIToken,
	).Scan(&enc); err != nil {
		t.Fatalf("query: %v", err)
	}
	if enc == "" {
		t.Error("value_encrypted is empty after SetSecret")
	}
	if enc == "supersecret-token-value" {
		t.Error("plaintext leaked into value_encrypted")
	}
}

func TestSet_OverwritesEncrypted(t *testing.T) {
	s, _ := newTestService(t)
	if err := s.SetSecret(context.Background(), "k", "secret-value"); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	if err := s.Set(context.Background(), "k", "plain-value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "plain-value" {
		t.Errorf("Get = %q, want plain-value", got)
	}
	enc, err := s.GetSecret(context.Background(), "k")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if enc != "" {
		t.Errorf("GetSecret = %q, want empty after plaintext overwrite", enc)
	}
}

func TestDelete(t *testing.T) {
	s, _ := newTestService(t)
	if err := s.Set(context.Background(), "k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Delete(context.Background(), "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err := s.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "" {
		t.Errorf("Get after Delete = %q, want empty", got)
	}
}

func TestSetSecret_WithoutSecretsService_Errors(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	s := New(d, nil)
	if err := s.SetSecret(context.Background(), "k", "v"); err == nil {
		t.Fatal("SetSecret without secrets service returned nil")
	}
	if _, err := s.GetSecret(context.Background(), "k"); err == nil {
		t.Fatal("GetSecret without secrets service returned nil")
	}
}
