package consolestore

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"carvilon.local/server/internal/db"
	"carvilon.local/server/internal/secrets"
)

func newTestStore(t *testing.T) (*Store, *db.DB) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	sec, err := secrets.NewWithKey(make([]byte, 32))
	if err != nil {
		t.Fatalf("secrets: %v", err)
	}
	return New(d.DB, sec), d
}

func TestCreateListGetDelete(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	p, err := s.CreateProfile(ctx, ProfileInput{
		Name: "VPS", Host: "vps.example", Port: 2222, Username: "root",
		AuthKind: AuthPassword, Secret: "hunter2",
	})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	if p.ID == 0 || !p.HasSecret || p.Port != 2222 {
		t.Fatalf("unexpected profile: %+v", p)
	}

	list, err := s.ListProfiles(ctx)
	if err != nil {
		t.Fatalf("ListProfiles: %v", err)
	}
	if len(list) != 1 || list[0].Name != "VPS" {
		t.Fatalf("list = %+v", list)
	}

	got, err := s.GetProfile(ctx, p.ID)
	if err != nil || got.Host != "vps.example" {
		t.Fatalf("GetProfile = %+v err=%v", got, err)
	}

	if err := s.DeleteProfile(ctx, p.ID); err != nil {
		t.Fatalf("DeleteProfile: %v", err)
	}
	if _, err := s.GetProfile(ctx, p.ID); err != ErrNotFound {
		t.Fatalf("after delete GetProfile err = %v, want ErrNotFound", err)
	}
}

// The plaintext secret must never appear in any read-back metadata, and
// must be stored encrypted at rest (not equal to the plaintext).
func TestSecretsNeverReturnedInClear(t *testing.T) {
	s, d := newTestStore(t)
	ctx := context.Background()
	const pw = "super-secret-password"

	p, err := s.CreateProfile(ctx, ProfileInput{
		Name: "box", Host: "h", Username: "u", AuthKind: AuthPassword, Secret: pw,
	})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}

	// Metadata (list + get) must only carry a "set" flag, never the value.
	list, _ := s.ListProfiles(ctx)
	if strings.Contains(strings.Join(dump(list), "|"), pw) {
		t.Fatal("plaintext password leaked into ListProfiles")
	}
	if !list[0].HasSecret {
		t.Fatal("HasSecret should be true")
	}

	// At rest the column holds ciphertext, not the plaintext.
	var stored string
	if err := d.QueryRow(`SELECT secret_enc FROM console_profiles WHERE id = ?`, p.ID).Scan(&stored); err != nil {
		t.Fatalf("read secret_enc: %v", err)
	}
	if stored == "" || stored == pw || strings.Contains(stored, pw) {
		t.Fatalf("secret stored in the clear: %q", stored)
	}

	// The dial-time accessor round-trips the plaintext.
	cred, err := s.ProfileCredential(ctx, p.ID)
	if err != nil {
		t.Fatalf("ProfileCredential: %v", err)
	}
	if cred.Password != pw {
		t.Fatalf("decrypted password = %q, want %q", cred.Password, pw)
	}
}

func dump(ps []Profile) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name+p.Host+p.Username+p.AuthKind)
	}
	return out
}

func TestKeyProfileAndPassphraseRoundTrip(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	const pem = "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----\n"

	p, err := s.CreateProfile(ctx, ProfileInput{
		Name: "pi", Host: "pi.local", Username: "pi", AuthKind: AuthKey,
		Secret: pem, Passphrase: "phrase",
	})
	if err != nil {
		t.Fatalf("CreateProfile key: %v", err)
	}
	if !p.HasPassphrase {
		t.Fatal("HasPassphrase should be true")
	}
	cred, err := s.ProfileCredential(ctx, p.ID)
	if err != nil {
		t.Fatalf("ProfileCredential: %v", err)
	}
	if string(cred.PrivateKey) != pem || cred.Passphrase != "phrase" || cred.AuthKind != AuthKey {
		t.Fatalf("credential round-trip mismatch: %+v", cred)
	}
}

// Updating without a new secret keeps the stored one; supplying one
// replaces it.
func TestUpdateKeepsSecretOnEmpty(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	p, _ := s.CreateProfile(ctx, ProfileInput{
		Name: "n", Host: "h", Username: "u", AuthKind: AuthPassword, Secret: "orig",
	})

	if err := s.UpdateProfile(ctx, p.ID, ProfileInput{
		Name: "n2", Host: "h2", Username: "u2", AuthKind: AuthPassword, Secret: "",
	}); err != nil {
		t.Fatalf("UpdateProfile keep: %v", err)
	}
	cred, _ := s.ProfileCredential(ctx, p.ID)
	if cred.Password != "orig" {
		t.Fatalf("secret should be kept, got %q", cred.Password)
	}
	got, _ := s.GetProfile(ctx, p.ID)
	if got.Name != "n2" || got.Host != "h2" {
		t.Fatalf("metadata not updated: %+v", got)
	}

	if err := s.UpdateProfile(ctx, p.ID, ProfileInput{
		Name: "n2", Host: "h2", Username: "u2", AuthKind: AuthPassword, Secret: "changed",
	}); err != nil {
		t.Fatalf("UpdateProfile replace: %v", err)
	}
	cred, _ = s.ProfileCredential(ctx, p.ID)
	if cred.Password != "changed" {
		t.Fatalf("secret should be replaced, got %q", cred.Password)
	}
}

func TestValidation(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	cases := []struct {
		name string
		in   ProfileInput
		want error
	}{
		{"empty name", ProfileInput{Host: "h", AuthKind: AuthPassword, Secret: "x"}, ErrEmptyName},
		{"empty host", ProfileInput{Name: "n", AuthKind: AuthPassword, Secret: "x"}, ErrEmptyHost},
		{"bad auth", ProfileInput{Name: "n", Host: "h", AuthKind: "otp", Secret: "x"}, ErrBadAuthKind},
		{"no secret", ProfileInput{Name: "n", Host: "h", AuthKind: AuthPassword}, ErrNoSecret},
	}
	for _, c := range cases {
		if _, err := s.CreateProfile(ctx, c.in); err != c.want {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
}

func TestHostKeyTOFU(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	const hp = "10.0.0.9:22"

	fp, err := s.LookupHostKey(ctx, hp)
	if err != nil || fp != "" {
		t.Fatalf("first lookup = %q err=%v, want empty", fp, err)
	}
	if err := s.PinHostKey(ctx, hp, "ssh-ed25519", "SHA256:aaa"); err != nil {
		t.Fatalf("PinHostKey: %v", err)
	}
	if fp, _ := s.LookupHostKey(ctx, hp); fp != "SHA256:aaa" {
		t.Fatalf("pinned = %q", fp)
	}
	// Re-pin (explicit re-trust) replaces.
	if err := s.PinHostKey(ctx, hp, "ssh-ed25519", "SHA256:bbb"); err != nil {
		t.Fatalf("re-pin: %v", err)
	}
	if fp, _ := s.LookupHostKey(ctx, hp); fp != "SHA256:bbb" {
		t.Fatalf("re-pinned = %q", fp)
	}
	if err := s.ForgetHostKey(ctx, hp); err != nil {
		t.Fatalf("ForgetHostKey: %v", err)
	}
	if fp, _ := s.LookupHostKey(ctx, hp); fp != "" {
		t.Fatalf("after forget = %q, want empty", fp)
	}
}
