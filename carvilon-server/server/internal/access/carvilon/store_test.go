package carvilon

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"carvilon.local/server/internal/access"
	"carvilon.local/server/internal/db"
)

func newTestStore(t *testing.T, opts ...Option) *Store {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return New(d.DB, opts...)
}

func ptrBool(b bool) *bool { return &b }

func TestCreate_AssignsStableUUIDAndDefaultsActive(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	u, err := s.Create(ctx, access.CreateNativeUserParams{DisplayName: "Sascha Daemgen"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if u.ID == "" {
		t.Fatal("Create returned empty ID")
	}
	if !u.Active {
		t.Errorf("new user Active = false, want true")
	}
	if u.DisplayName != "Sascha Daemgen" {
		t.Errorf("DisplayName = %q", u.DisplayName)
	}

	// ID muss ueber ein Re-Get stabil bleiben.
	got, err := s.Get(ctx, u.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != u.ID {
		t.Errorf("Get ID = %q, want %q", got.ID, u.ID)
	}
	if got.DisplayName != "Sascha Daemgen" {
		t.Errorf("Get DisplayName = %q", got.DisplayName)
	}
}

func TestCreate_TrimsAndRejectsEmpty(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.Create(ctx, access.CreateNativeUserParams{DisplayName: "   "}); err == nil {
		t.Error("Create with blank name should fail")
	}

	u, err := s.Create(ctx, access.CreateNativeUserParams{DisplayName: "  Anna  "})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if u.DisplayName != "Anna" {
		t.Errorf("DisplayName = %q, want trimmed 'Anna'", u.DisplayName)
	}
}

func TestCreate_UniqueIDs(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		u, err := s.Create(ctx, access.CreateNativeUserParams{DisplayName: "User"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if seen[u.ID] {
			t.Fatalf("duplicate ID %q", u.ID)
		}
		seen[u.ID] = true
	}
}

func TestCreate_StoresOptionalUALink(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	linked, err := s.Create(ctx, access.CreateNativeUserParams{DisplayName: "Linked", UAUserID: "ua-123"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if linked.UAUserID != "ua-123" {
		t.Errorf("UAUserID = %q, want ua-123", linked.UAUserID)
	}
	got, _ := s.Get(ctx, linked.ID)
	if got.UAUserID != "ua-123" {
		t.Errorf("persisted UAUserID = %q", got.UAUserID)
	}

	// Ohne Kopplung bleibt das Feld leer (SQL NULL).
	plain, _ := s.Create(ctx, access.CreateNativeUserParams{DisplayName: "Plain"})
	got2, _ := s.Get(ctx, plain.ID)
	if got2.UAUserID != "" {
		t.Errorf("unlinked UAUserID = %q, want empty", got2.UAUserID)
	}
}

func TestGet_NotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.Get(ctx, "does-not-exist"); !errors.Is(err, access.ErrNotFound) {
		t.Errorf("Get unknown = %v, want ErrNotFound", err)
	}
}

func TestUpdate_ChangesNameKeepsIDAndActive(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u, _ := s.Create(ctx, access.CreateNativeUserParams{DisplayName: "Old Name"})
	if err := s.SetActive(ctx, u.ID, false); err != nil {
		t.Fatalf("SetActive: %v", err)
	}

	updated, err := s.Update(ctx, u.ID, access.UpdateNativeUserParams{DisplayName: "New Name"})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.ID != u.ID {
		t.Errorf("Update changed ID: %q != %q", updated.ID, u.ID)
	}
	if updated.DisplayName != "New Name" {
		t.Errorf("DisplayName = %q", updated.DisplayName)
	}
	if updated.Active {
		t.Errorf("Update flipped Active; want still inactive")
	}
}

func TestUpdate_RejectsEmptyAndUnknown(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u, _ := s.Create(ctx, access.CreateNativeUserParams{DisplayName: "Name"})

	if _, err := s.Update(ctx, u.ID, access.UpdateNativeUserParams{DisplayName: " "}); err == nil {
		t.Error("Update with blank name should fail")
	}
	if _, err := s.Update(ctx, "nope", access.UpdateNativeUserParams{DisplayName: "X"}); !errors.Is(err, access.ErrNotFound) {
		t.Errorf("Update unknown = %v, want ErrNotFound", err)
	}
}

func TestSetActive_Toggles(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u, _ := s.Create(ctx, access.CreateNativeUserParams{DisplayName: "Toggle"})

	if err := s.SetActive(ctx, u.ID, false); err != nil {
		t.Fatalf("SetActive false: %v", err)
	}
	got, _ := s.Get(ctx, u.ID)
	if got.Active {
		t.Error("after SetActive(false), Active = true")
	}

	if err := s.SetActive(ctx, u.ID, true); err != nil {
		t.Fatalf("SetActive true: %v", err)
	}
	got, _ = s.Get(ctx, u.ID)
	if !got.Active {
		t.Error("after SetActive(true), Active = false")
	}

	if err := s.SetActive(ctx, "nope", true); !errors.Is(err, access.ErrNotFound) {
		t.Errorf("SetActive unknown = %v, want ErrNotFound", err)
	}
}

func TestDelete_RemovesAndIsNotFoundAfter(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u, _ := s.Create(ctx, access.CreateNativeUserParams{DisplayName: "Bye"})

	if err := s.Delete(ctx, u.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, u.ID); !errors.Is(err, access.ErrNotFound) {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, u.ID); !errors.Is(err, access.ErrNotFound) {
		t.Errorf("second Delete = %v, want ErrNotFound", err)
	}
}

func TestList_SortsActiveFirstThenName(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	zora, _ := s.Create(ctx, access.CreateNativeUserParams{DisplayName: "Zora"})
	_, _ = s.Create(ctx, access.CreateNativeUserParams{DisplayName: "anna"})
	bea, _ := s.Create(ctx, access.CreateNativeUserParams{DisplayName: "Bea"})
	// Zora inaktiv -> muss trotz Z-Name aber wegen inaktiv nach hinten.
	_ = s.SetActive(ctx, zora.ID, false)
	_ = bea // keep referenced

	all, err := s.List(ctx, access.NativeListParams{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("List len = %d, want 3", len(all))
	}
	// aktive zuerst, case-insensitiv alphabetisch: anna, Bea, dann Zora (inaktiv).
	wantOrder := []string{"anna", "Bea", "Zora"}
	for i, w := range wantOrder {
		if all[i].DisplayName != w {
			t.Errorf("order[%d] = %q, want %q (full: %v)", i, all[i].DisplayName, w, names(all))
		}
	}
}

func TestList_FiltersByActiveAndQuery(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a, _ := s.Create(ctx, access.CreateNativeUserParams{DisplayName: "Sascha Daemgen"})
	_, _ = s.Create(ctx, access.CreateNativeUserParams{DisplayName: "Anna Mueller"})
	_ = s.SetActive(ctx, a.ID, false)

	activeOnly, _ := s.List(ctx, access.NativeListParams{Active: ptrBool(true)})
	if len(activeOnly) != 1 || activeOnly[0].DisplayName != "Anna Mueller" {
		t.Errorf("active filter = %v, want [Anna Mueller]", names(activeOnly))
	}
	inactiveOnly, _ := s.List(ctx, access.NativeListParams{Active: ptrBool(false)})
	if len(inactiveOnly) != 1 || inactiveOnly[0].DisplayName != "Sascha Daemgen" {
		t.Errorf("inactive filter = %v, want [Sascha Daemgen]", names(inactiveOnly))
	}
	byName, _ := s.List(ctx, access.NativeListParams{Query: "mueller"})
	if len(byName) != 1 || byName[0].DisplayName != "Anna Mueller" {
		t.Errorf("query filter = %v, want [Anna Mueller]", names(byName))
	}
	none, _ := s.List(ctx, access.NativeListParams{Query: "zzz"})
	if len(none) != 0 {
		t.Errorf("no-match query returned %d rows", len(none))
	}
}

func TestList_EmptyReturnsNonNilEmptySlice(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	got, err := s.List(ctx, access.NativeListParams{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got == nil {
		t.Error("List returned nil slice; want non-nil empty")
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestTimestamps_UpdateOnMutation(t *testing.T) {
	ctx := context.Background()
	now := int64(1_700_000_000_000)
	clock := func() time.Time { return time.UnixMilli(now) }
	s := newTestStore(t, WithClock(clock))

	u, _ := s.Create(ctx, access.CreateNativeUserParams{DisplayName: "Clocked"})
	if !u.CreatedAt.Equal(time.UnixMilli(1_700_000_000_000)) {
		t.Errorf("CreatedAt = %v", u.CreatedAt)
	}

	now = 1_700_000_005_000
	updated, _ := s.Update(ctx, u.ID, access.UpdateNativeUserParams{DisplayName: "Clocked2"})
	if !updated.UpdatedAt.Equal(time.UnixMilli(1_700_000_005_000)) {
		t.Errorf("UpdatedAt after update = %v, want advanced", updated.UpdatedAt)
	}
	if !updated.CreatedAt.Equal(time.UnixMilli(1_700_000_000_000)) {
		t.Errorf("CreatedAt drifted to %v", updated.CreatedAt)
	}
}

func TestWithIDFunc_Deterministic(t *testing.T) {
	ctx := context.Background()
	ids := []string{"id-a", "id-b"}
	i := 0
	s := newTestStore(t, WithIDFunc(func() string {
		id := ids[i%len(ids)]
		i++
		return id
	}))
	u1, _ := s.Create(ctx, access.CreateNativeUserParams{DisplayName: "A"})
	if u1.ID != "id-a" {
		t.Errorf("ID = %q, want id-a", u1.ID)
	}
}

func names(us []access.NativeUser) []string {
	out := make([]string, len(us))
	for i, u := range us {
		out[i] = u.DisplayName
	}
	return out
}
