package mqttstore

import (
	"context"
	"path/filepath"
	"testing"

	"carvilon.local/server/internal/db"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	pepper := func(context.Context) (string, error) { return "test-pepper", nil }
	return New(d.DB, pepper)
}

func TestCreateAuthenticateDevice(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.CreateDevice(ctx, "flur", "supersecret", "Flur EG"); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	// Duplicate rejected.
	if err := s.CreateDevice(ctx, "flur", "another1234", ""); err != ErrDeviceExists {
		t.Fatalf("duplicate CreateDevice = %v, want ErrDeviceExists", err)
	}
	// Short password rejected.
	if err := s.CreateDevice(ctx, "x", "short", ""); err != ErrPasswordTooShort {
		t.Fatalf("short password = %v, want ErrPasswordTooShort", err)
	}
	// Bad username rejected.
	if err := s.CreateDevice(ctx, "bad/name", "supersecret", ""); err != ErrInvalidUsername {
		t.Fatalf("bad username = %v, want ErrInvalidUsername", err)
	}

	az, err := s.LoadAuthz(ctx)
	if err != nil {
		t.Fatalf("LoadAuthz: %v", err)
	}
	if !az.Authenticate("flur", "supersecret") {
		t.Error("valid credentials should authenticate")
	}
	if az.Authenticate("flur", "wrongpass") {
		t.Error("wrong password must not authenticate")
	}
	if az.Authenticate("ghost", "supersecret") {
		t.Error("unknown user must not authenticate")
	}
}

func TestACLRoundTripAndLoad(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.CreateDevice(ctx, "flur", "supersecret", ""); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	if err := s.AddACL(ctx, "flur", "publish", "shared/cmd", true); err != nil {
		t.Fatalf("AddACL: %v", err)
	}
	if err := s.AddACL(ctx, "flur", "both", "carvilon/flur/secret", false); err != nil {
		t.Fatalf("AddACL deny: %v", err)
	}
	// ACL for missing device -> not found.
	if err := s.AddACL(ctx, "ghost", "publish", "x", true); err != ErrDeviceNotFound {
		t.Fatalf("AddACL ghost = %v, want ErrDeviceNotFound", err)
	}
	// Bad filter rejected.
	if err := s.AddACL(ctx, "flur", "publish", "a/#/b", true); err != ErrInvalidACL {
		t.Fatalf("AddACL bad filter = %v, want ErrInvalidACL", err)
	}

	rules, err := s.ListACL(ctx, "flur")
	if err != nil {
		t.Fatalf("ListACL: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("ListACL len = %d, want 2", len(rules))
	}

	az, err := s.LoadAuthz(ctx)
	if err != nil {
		t.Fatalf("LoadAuthz: %v", err)
	}
	if !az.Allowed("flur", "shared/cmd", true) {
		t.Error("explicit allow should permit")
	}
	if az.Allowed("flur", "carvilon/flur/secret", true) {
		t.Error("explicit deny should block even own subtree")
	}

	// Delete cascade: removing the device drops its ACL rows.
	if err := s.DeleteDevice(ctx, "flur"); err != nil {
		t.Fatalf("DeleteDevice: %v", err)
	}
	rules, err = s.ListACL(ctx, "flur")
	if err != nil {
		t.Fatalf("ListACL after delete: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("ACL rows after device delete = %d, want 0", len(rules))
	}
}
