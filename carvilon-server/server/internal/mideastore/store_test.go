package mideastore

import (
	"bytes"
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"carvilon.local/server/internal/db"
	"carvilon.local/server/internal/secrets"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// newTestStore opens a real temp-file DB so the full migration stack (including
// migration 042, midea_devices) runs, with a real secrets service.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	sec, err := secrets.NewWithKey(key)
	if err != nil {
		t.Fatalf("secrets.NewWithKey: %v", err)
	}
	c := &clock{t: time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)}
	return New(d.DB, sec, WithClock(c.now))
}

// TestDiscoverThenApprove: a discovered device lands pending, and approval
// persists encrypted credentials that round-trip back byte-for-byte.
func TestDiscoverThenApprove(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	dev, err := s.InsertDiscovered(ctx, Detected{DeviceID: 0xABCDEF, Address: "192.0.2.10", Name: "net_ac_test", ProtocolV3: true})
	if err != nil {
		t.Fatalf("InsertDiscovered: %v", err)
	}
	if dev.State != StatePending || dev.HasCreds || !dev.ProtocolV3 {
		t.Fatalf("fresh device = %+v, want pending/no-creds/v3", dev)
	}
	if dev.ID != "abcdef" {
		t.Fatalf("id = %q, want abcdef", dev.ID)
	}

	pending, err := s.ListPending(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("ListPending = %d (%v), want 1", len(pending), err)
	}

	token := []byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x11}
	key := []byte{0x01, 0x02, 0x03, 0x04}
	if err := s.Approve(ctx, dev.ID, token, key, ProfileStandard); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	active, err := s.ListActive(ctx)
	if err != nil || len(active) != 1 {
		t.Fatalf("ListActive = %d (%v), want 1", len(active), err)
	}
	if !active[0].HasCreds || active[0].State != StateActive {
		t.Fatalf("approved device = %+v, want active/has-creds", active[0])
	}

	gotTok, gotKey, err := s.Credential(ctx, dev.ID)
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if !bytes.Equal(gotTok, token) || !bytes.Equal(gotKey, key) {
		t.Fatalf("credential round-trip mismatch: token %x/%x key %x/%x", gotTok, token, gotKey, key)
	}
}

// TestProfileToggle: standard is the default and the toggle persists.
func TestProfileToggle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	dev, _ := s.InsertDiscovered(ctx, Detected{DeviceID: 42, Address: "192.0.2.11", ProtocolV3: true})
	if err := s.Approve(ctx, dev.ID, []byte{1}, []byte{2}, ProfileStandard); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := s.SetProfile(ctx, dev.ID, ProfileAdvanced); err != nil {
		t.Fatalf("SetProfile: %v", err)
	}
	got, _ := s.Get(ctx, dev.ID)
	if got.Profile != ProfileAdvanced {
		t.Fatalf("profile = %q, want advanced", got.Profile)
	}
	// Unknown profile clamps to standard.
	_ = s.SetProfile(ctx, dev.ID, "bogus")
	got, _ = s.Get(ctx, dev.ID)
	if got.Profile != ProfileStandard {
		t.Fatalf("profile = %q, want standard after bogus", got.Profile)
	}
}

// TestApproveGuardsAgainstRejected: an approval whose (slow cloud-pairing)
// window raced a concurrent Reject must NOT silently un-ignore the device.
func TestApproveGuardsAgainstRejected(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	dev, _ := s.InsertDiscovered(ctx, Detected{DeviceID: 99, Address: "192.0.2.20", ProtocolV3: true})
	// A concurrent Reject lands while the approver is still pairing.
	if err := s.Reject(ctx, dev.ID); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	// The stale Approve must affect 0 rows (device is 'ignored', not 'pending').
	if err := s.Approve(ctx, dev.ID, []byte{1, 2}, []byte{3, 4}, ProfileStandard); err != ErrNotFound {
		t.Fatalf("Approve on ignored device = %v, want ErrNotFound", err)
	}
	got, _ := s.Get(ctx, dev.ID)
	if got.State != StateIgnored || got.HasCreds {
		t.Fatalf("device after stale approve = %+v, want ignored / no creds", got)
	}
}

// TestRejectStickyThenRelease: a rejected device is ignored (sticky) and a
// re-discovery does not revert it to pending; release removes it.
func TestRejectStickyThenRelease(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	dev, _ := s.InsertDiscovered(ctx, Detected{DeviceID: 7, Address: "192.0.2.12", ProtocolV3: true})
	if err := s.Reject(ctx, dev.ID); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	// Re-discovery must NOT flip it back to pending.
	if _, err := s.InsertDiscovered(ctx, Detected{DeviceID: 7, Address: "192.0.2.99", ProtocolV3: true}); err != nil {
		t.Fatalf("re-InsertDiscovered: %v", err)
	}
	got, _ := s.Get(ctx, dev.ID)
	if got.State != StateIgnored {
		t.Fatalf("state = %q, want ignored (sticky)", got.State)
	}
	if got.Address != "192.0.2.99" {
		t.Fatalf("address = %q, want refreshed to .99", got.Address)
	}
	if err := s.Release(ctx, dev.ID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := s.Get(ctx, dev.ID); err != ErrNotFound {
		t.Fatalf("Get after release = %v, want ErrNotFound", err)
	}
}
