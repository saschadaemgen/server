package shellystore

import (
	"context"
	"errors"
	"testing"
)

// Tests for the generation axis (migration 040): the gen column feeds the
// adapter dispatch (Gen2+ RPC vs Gen1 REST), so a wrong or lost generation
// silently talks the wrong protocol to a device. Addresses use the RFC5737
// TEST-NET ranges so no test value ever collides with a real LAN.

// TestAdoptInsertsGen: a fresh adopt persists the announced generation for
// both Gen1 and Gen2 devices - the fleet must know the API family from the
// first announcement onward.
func TestAdoptInsertsGen(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if _, err := s.Adopt(ctx, Detected{MAC: "AAAAAAAAAAAA", Address: "192.0.2.10", Gen: Gen1}, capN, true); err != nil {
		t.Fatalf("adopt gen1: %v", err)
	}
	if _, err := s.Adopt(ctx, Detected{MAC: "BBBBBBBBBBBB", Address: "192.0.2.11", Gen: Gen2}, capN, true); err != nil {
		t.Fatalf("adopt gen2: %v", err)
	}
	active, err := s.ListActive(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	m := addrs(active)
	if got := m["192.0.2.10"].Gen; got != Gen1 {
		t.Fatalf("gen1 device stored gen = %d, want %d", got, Gen1)
	}
	if got := m["192.0.2.11"].Gen; got != Gen2 {
		t.Fatalf("gen2 device stored gen = %d, want %d", got, Gen2)
	}
}

// TestAdoptGenRefresh: a re-announcement WITHOUT a generation (Gen=0, e.g. a
// source that could not classify) must keep the stored value - the store
// never downgrades a known generation to unknown. A re-announcement with a
// DIFFERENT generation corrects the stored one (the announcement is the
// fresher truth, e.g. after a first misclassification).
func TestAdoptGenRefresh(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if _, err := s.Adopt(ctx, Detected{MAC: "AAAAAAAAAAAA", Address: "192.0.2.10", Gen: Gen2}, capN, true); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	// Gen=0 re-announcement (MAC-match refresh path): stored gen survives.
	res, err := s.Adopt(ctx, Detected{MAC: "AAAAAAAAAAAA", Address: "192.0.2.10"}, capN, true)
	if err != nil {
		t.Fatalf("re-adopt gen0: %v", err)
	}
	if res != AdoptedKnown {
		t.Fatalf("re-adopt = %v, want AdoptedKnown", res)
	}
	active, _ := s.ListActive(ctx)
	if len(active) != 1 || active[0].Gen != Gen2 {
		t.Fatalf("gen after Gen=0 refresh = %+v, want kept Gen2", active)
	}
	// A different generation in the announcement wins over the stored one.
	if _, err := s.Adopt(ctx, Detected{MAC: "AAAAAAAAAAAA", Address: "192.0.2.10", Gen: Gen1}, capN, true); err != nil {
		t.Fatalf("re-adopt gen1: %v", err)
	}
	active, _ = s.ListActive(ctx)
	if len(active) != 1 || active[0].Gen != Gen1 {
		t.Fatalf("gen after Gen1 refresh = %+v, want updated Gen1", active)
	}
	// Same on the address-only refresh path (announcement without a MAC).
	if _, err := s.Adopt(ctx, Detected{Address: "192.0.2.10", Gen: Gen2}, capN, true); err != nil {
		t.Fatalf("re-adopt by address: %v", err)
	}
	active, _ = s.ListActive(ctx)
	if len(active) != 1 || active[0].Gen != Gen2 {
		t.Fatalf("gen after address-only refresh = %+v, want Gen2", active)
	}
}

// TestAdoptMacUpgradeCarriesGen: when discovery upgrades a mac-less manual
// pin in place (fills the MAC instead of duplicating the row), the announced
// generation must ride along - otherwise a manual Gen1 device would stay
// "unknown" forever despite having been identified.
func TestAdoptMacUpgradeCarriesGen(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if err := s.ReplaceManual(ctx, []string{"192.0.2.20"}); err != nil {
		t.Fatalf("replace manual: %v", err)
	}
	res, err := s.Adopt(ctx, Detected{MAC: "AAAAAAAAAAAA", Address: "192.0.2.20", Gen: Gen1}, capN, true)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if res != AdoptedKnown {
		t.Fatalf("adopt of manual addr = %v, want AdoptedKnown", res)
	}
	active, _ := s.ListActive(ctx)
	if len(active) != 1 {
		t.Fatalf("active = %d, want 1 (duplicate created?)", len(active))
	}
	if active[0].MAC != "AAAAAAAAAAAA" || active[0].Gen != Gen1 {
		t.Fatalf("upgraded row = MAC %q gen %d, want the discovered MAC + Gen1", active[0].MAC, active[0].Gen)
	}
}

// TestSetIdentity: an identify probe fills in gen, model and MAC - but each
// field only under its own rule: gen only when > 0, model only when
// non-empty, and the MAC only into an EMPTY slot (a probe must never
// overwrite an identity the row already has).
func TestSetIdentity(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// A mac-less manual pin: exactly the row an identify probe classifies.
	if err := s.ReplaceManual(ctx, []string{"192.0.2.30"}); err != nil {
		t.Fatalf("replace manual: %v", err)
	}
	active, _ := s.ListActive(ctx)
	id := active[0].ID

	if err := s.SetIdentity(ctx, id, "AAAAAAAAAAAA", "SHSW-25", Gen1); err != nil {
		t.Fatalf("set identity: %v", err)
	}
	got, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.MAC != "AAAAAAAAAAAA" || got.Model != "SHSW-25" || got.Gen != Gen1 {
		t.Fatalf("after probe = MAC %q model %q gen %d, want all three set", got.MAC, got.Model, got.Gen)
	}

	// A no-op probe result (gen 0, empty model, empty MAC) keeps everything.
	if err := s.SetIdentity(ctx, id, "", "", GenUnknown); err != nil {
		t.Fatalf("set identity empty: %v", err)
	}
	got, _ = s.Get(ctx, id)
	if got.MAC != "AAAAAAAAAAAA" || got.Model != "SHSW-25" || got.Gen != Gen1 {
		t.Fatalf("empty probe changed the row: MAC %q model %q gen %d", got.MAC, got.Model, got.Gen)
	}

	// A later probe may correct gen and model, but the MAC slot is taken:
	// the existing identity is never overwritten.
	if err := s.SetIdentity(ctx, id, "CCCCCCCCCCCC", "Shelly Pro4PM", Gen2); err != nil {
		t.Fatalf("set identity again: %v", err)
	}
	got, _ = s.Get(ctx, id)
	if got.Gen != Gen2 || got.Model != "Shelly Pro4PM" {
		t.Fatalf("re-probe = model %q gen %d, want updated", got.Model, got.Gen)
	}
	if got.MAC != "AAAAAAAAAAAA" {
		t.Fatalf("re-probe overwrote the MAC: %q, want the original", got.MAC)
	}
}

// TestSetIdentityMACConflictSkipped: when the probed MAC already belongs to
// another row (the same physical device known under a second address), the
// MAC fill is skipped - gen/model still land, and no unique-index violation
// surfaces. Reconciling the two rows is Adopt's job, not the probe's.
func TestSetIdentityMACConflictSkipped(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// Row A owns the MAC; row B is a mac-less manual pin at another address.
	if _, err := s.Adopt(ctx, Detected{MAC: "AAAAAAAAAAAA", Address: "192.0.2.40", Gen: Gen2}, capN, true); err != nil {
		t.Fatalf("adopt A: %v", err)
	}
	if err := s.ReplaceManual(ctx, []string{"192.0.2.41"}); err != nil {
		t.Fatalf("replace manual: %v", err)
	}
	active, _ := s.ListActive(ctx)
	b := addrs(active)["192.0.2.41"]

	if err := s.SetIdentity(ctx, b.ID, "AAAAAAAAAAAA", "SHSW-1", Gen1); err != nil {
		t.Fatalf("set identity with taken MAC: %v", err)
	}
	got, _ := s.Get(ctx, b.ID)
	if got.MAC != "" {
		t.Fatalf("row B stole a claimed MAC: %q, want empty", got.MAC)
	}
	if got.Model != "SHSW-1" || got.Gen != Gen1 {
		t.Fatalf("conflict skip dropped model/gen: model %q gen %d", got.Model, got.Gen)
	}
	// Row A's identity is untouched by B's probe.
	a, _ := s.Get(ctx, addrs(active)["192.0.2.40"].ID)
	if a.MAC != "AAAAAAAAAAAA" || a.Gen != Gen2 {
		t.Fatalf("row A changed: MAC %q gen %d", a.MAC, a.Gen)
	}
}

// TestSetIdentityNotFound: an unknown id is reported, not silently ignored -
// the identify loop must notice a device that vanished mid-probe.
func TestSetIdentityNotFound(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	if err := s.SetIdentity(ctx, 12345, "AAAAAAAAAAAA", "SHSW-1", Gen1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("set identity on unknown id = %v, want ErrNotFound", err)
	}
}

// TestManualRowDefaultsGenUnknown: a manual pin inserted without any probe
// contact reads GenUnknown - the migration-040 column default. (The 040
// backfill itself cannot be exercised here: newTestStore runs the full
// migration stack, so no pre-040 row can exist. The default is the half of
// the migration the store API can observe: ReplaceManual's INSERT names no
// gen column, so the row's 0 comes from the schema, never a guess.)
func TestManualRowDefaultsGenUnknown(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if err := s.ReplaceManual(ctx, []string{"192.0.2.50"}); err != nil {
		t.Fatalf("replace manual: %v", err)
	}
	active, _ := s.ListActive(ctx)
	if len(active) != 1 {
		t.Fatalf("active = %d, want 1", len(active))
	}
	if active[0].Gen != GenUnknown {
		t.Fatalf("manual row gen = %d, want GenUnknown (column default)", active[0].Gen)
	}
}

// TestAdoptRefreshPreservesNameOnEmpty: a re-find with empty Name/Model
// (e.g. a scan responder that reports no app/model) must NOT blank the
// name/model an earlier mDNS announcement stored. Only a non-empty
// incoming value replaces it - matching the gen guard.
func TestAdoptRefreshPreservesNameOnEmpty(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// mDNS-style adopt with a real name + model.
	if _, err := s.Adopt(ctx, Detected{
		MAC: "AABBCCDDEEFF", Address: "192.168.1.50", Name: "Kitchen 1PM",
		Model: "Shelly Plus1PM", Gen: Gen2,
	}, capN, true); err != nil {
		t.Fatal(err)
	}
	// A later find (same MAC) carrying NO name/model - must preserve both.
	if res, err := s.Adopt(ctx, Detected{
		MAC: "AABBCCDDEEFF", Address: "192.168.1.51", Name: "", Model: "", Gen: 0,
	}, capN, true); err != nil || res != AdoptedKnown {
		t.Fatalf("re-adopt = %v, %v", res, err)
	}
	active, err := s.ListActive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := addrs(active)["192.168.1.51"]
	if got.Name != "Kitchen 1PM" || got.Model != "Shelly Plus1PM" || got.Gen != Gen2 {
		t.Errorf("empty re-find blanked identity: name=%q model=%q gen=%d", got.Name, got.Model, got.Gen)
	}
	// A find WITH a new name still updates it.
	if _, err := s.Adopt(ctx, Detected{
		MAC: "AABBCCDDEEFF", Address: "192.168.1.51", Name: "Kitchen main", Model: "Shelly Plus1PM", Gen: Gen2,
	}, capN, true); err != nil {
		t.Fatal(err)
	}
	active, err = s.ListActive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := addrs(active)["192.168.1.51"]; got.Name != "Kitchen main" {
		t.Errorf("non-empty name not applied: %q", got.Name)
	}
}
