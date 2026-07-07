// Package shellystore is the persistence layer for the Shelly device set
// (migration 038): the single source of truth for which Shelly devices
// CARVILON polls, plus the sticky ignore list behind manual removal.
//
// Shelly Etappe 2 lifts the Etappe-1 model (one comma-separated address
// string in platform_config) to a real table so manual IPs and mDNS-
// discovered devices can share one set with a per-device origin and a
// durable identity (MAC). It is the single SQL writer for shelly_devices.
//
// The two axes:
//
//   - origin: 'manual' (typed into the settings IP list) vs 'discovered'
//     (found via mDNS). Both are polled identically; the origin is only
//     for display and for the manual-list reconciliation (ReplaceManual).
//   - state: 'active' (polled + shown in the Device Center) vs 'ignored'
//     (manually removed - never auto-adopted again until released). An
//     ignored row is KEPT, not deleted, so discovery recognises the
//     device by MAC or address and skips it (the sticky behaviour that
//     keeps a removed device gone instead of instantly reappearing).
//
// Identity for the ignore list is primarily the MAC (durable across a
// DHCP address change); a manual IP that was never reached has no MAC, so
// it is ignored by its configured address instead. Adopt reconciles both.
//
// Nothing here ever writes to a device: removal is a CARVILON-side config
// action (we forget the device in OUR list), so device control stays
// read-only exactly as in Etappe 1.
package shellystore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound is returned when a device id/address has no matching row.
var ErrNotFound = errors.New("shellystore: device not found")

// ErrAtCap is returned when approving would push the active set past its cap.
var ErrAtCap = errors.New("shellystore: active device cap reached")

// Origins and states as stored in the table.
const (
	OriginManual     = "manual"
	OriginDiscovered = "discovered"
	StateActive      = "active"
	StateIgnored     = "ignored"
	// StatePending is a device found by discovery while the approval gate is
	// on: a stored record only. A pending device is NEVER polled, gets NO
	// request/credentials and is NOT in the active client fleet - it waits
	// for the operator to Approve (-> active) or Reject (-> ignored). We do
	// not talk to a device before a human approves it.
	StatePending = "pending"
)

// Store is the SQL gateway for the Shelly device set.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Option mutates a Store during construction.
type Option func(*Store)

// WithClock injects a test clock.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// New constructs a Store.
func New(db *sql.DB, opts ...Option) *Store {
	s := &Store{db: db, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Device is one row of the Shelly device set.
type Device struct {
	ID          int64
	MAC         string // normalised device id/MAC (uppercase hex, no separators); "" if unknown
	Address     string // canonical LAN IPv4[:port]
	Origin      string // OriginManual | OriginDiscovered
	State       string // StateActive | StateIgnored
	Name        string // last-seen display name; "" allowed
	Model       string // last-seen model; "" allowed
	FirstSeenAt int64  // ms epoch
	UpdatedAt   int64  // ms epoch
}

// Detected is one device an announcement (or a probe) reported, in the
// neutral shape Adopt understands. Address must already be a canonical,
// LAN-guarded form - the store does not re-validate it.
type Detected struct {
	MAC     string // normalised; "" if unknown
	Address string // canonical LAN IPv4[:port]
	Name    string
	Model   string
}

// ListActive returns every device that is currently polled + shown,
// ordered for stable display.
func (s *Store) ListActive(ctx context.Context) ([]Device, error) {
	return s.query(ctx, `WHERE state = ? ORDER BY address, id`, StateActive)
}

// ListIgnored returns the ignore list (the sticky-removed devices),
// most recently ignored first.
func (s *Store) ListIgnored(ctx context.Context) ([]Device, error) {
	return s.query(ctx, `WHERE state = ? ORDER BY updated_at DESC, id`, StateIgnored)
}

// ListManualActive returns the active, manually-configured devices - the
// backing set for the settings IP list. Ordered by address.
func (s *Store) ListManualActive(ctx context.Context) ([]Device, error) {
	return s.query(ctx, `WHERE state = ? AND origin = ? ORDER BY address, id`, StateActive, OriginManual)
}

func (s *Store) query(ctx context.Context, whereOrder string, args ...any) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, mac, address, origin, state, name, model, first_seen_at, updated_at
		   FROM shelly_devices `+whereOrder, args...)
	if err != nil {
		return nil, fmt.Errorf("shellystore: list: %w", err)
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.MAC, &d.Address, &d.Origin, &d.State,
			&d.Name, &d.Model, &d.FirstSeenAt, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("shellystore: scan: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("shellystore: rows: %w", err)
	}
	return out, nil
}

// CountActive returns how many devices are currently active (for the cap
// check and the "configured?" default of the enabled toggle).
func (s *Store) CountActive(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM shelly_devices WHERE state = ?`, StateActive).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("shellystore: count active: %w", err)
	}
	return n, nil
}

// AdoptResult is Adopt's outcome for logging/UI without leaking the
// address into a caller that logs the result.
type AdoptResult int

const (
	// AdoptedNew - a fresh device was auto-activated (approval gate off).
	AdoptedNew AdoptResult = iota
	// AdoptedPending - a fresh device was recorded as pending approval
	// (approval gate on, the default). It is NOT polled and NOT in the fleet.
	AdoptedPending
	// AdoptedKnown - the device was already configured (active or pending);
	// only its last-seen fields were refreshed, state unchanged.
	AdoptedKnown
	// AdoptSkippedIgnored - the device is on the ignore list; skipped.
	AdoptSkippedIgnored
	// AdoptSkippedFull - the target set (active, or pending) is at the cap;
	// skipped.
	AdoptSkippedFull
)

// Adopt folds one discovered device into the set, honouring the approval
// gate:
//
//   - Ignored (by MAC when known, else the exact address) -> skipped
//     (AdoptSkippedIgnored). The sticky guarantee: a removed device stays
//     gone.
//   - Already known (a non-ignored row - active OR pending - by MAC, or by
//     address when the MAC is unknown) -> its address/name/model are
//     refreshed in place, state UNCHANGED (a pending device is never
//     silently activated by a re-announcement): AdoptedKnown. A discovered
//     MAC also fills in a matching mac-less row so the two never split.
//   - Otherwise a fresh device: when autoAdopt is true (the gate is off) it
//     is inserted active (AdoptedNew, joins the fleet); when false (the
//     default gate) it is inserted pending (AdoptedPending, stored only,
//     never polled). Either way the target set is capped (AdoptSkippedFull)
//     so an announcement flood cannot blow the list up.
//
// limit caps the target set (active OR pending). A MAC, when present, must be
// pre-normalised by the caller.
func (s *Store) Adopt(ctx context.Context, d Detected, limit int, autoAdopt bool) (AdoptResult, error) {
	if d.Address == "" {
		return AdoptSkippedFull, errors.New("shellystore: adopt without address")
	}
	now := s.now().UnixMilli()

	// 1. Ignore list wins over everything (sticky removal). Match on MAC
	//    first (durable across DHCP), then on the exact address.
	ignored, err := s.isIgnored(ctx, d.MAC, d.Address)
	if err != nil {
		return AdoptSkippedFull, err
	}
	if ignored {
		return AdoptSkippedIgnored, nil
	}

	// 2. Already known among the NON-ignored rows (active or pending)? A MAC
	//    match is authoritative (the MAC is globally unique); without a MAC,
	//    match the address. Refresh in place, never touching the state.
	if d.MAC != "" {
		var id int64
		var rowState string
		err := s.db.QueryRowContext(ctx,
			`SELECT id, state FROM shelly_devices WHERE mac = ? AND state <> ?`, d.MAC, StateIgnored).Scan(&id, &rowState)
		switch {
		case err == nil:
			// The device now lives at d.Address; drop the stale occupant of
			// that address so a DHCP move never leaves two rows at one
			// address. Crucially, a device MOVING as pending only evicts
			// another PENDING occupant - an unapproved find must never delete
			// an approved (active) device that happens to share the IP.
			if err := s.clearAddressForState(ctx, d.Address, rowState, id); err != nil {
				return AdoptSkippedFull, err
			}
			_, err = s.db.ExecContext(ctx,
				`UPDATE shelly_devices SET address = ?, name = ?, model = ?, updated_at = ? WHERE id = ?`,
				d.Address, d.Name, d.Model, now, id)
			if err != nil {
				return AdoptSkippedFull, fmt.Errorf("shellystore: adopt refresh: %w", err)
			}
			return AdoptedKnown, nil
		case !errors.Is(err, sql.ErrNoRows):
			return AdoptSkippedFull, fmt.Errorf("shellystore: adopt mac lookup: %w", err)
		}
		// No MAC row yet: a mac-less non-ignored row at this address (a manual
		// pin, or an earlier pending find without a parseable MAC) is the same
		// physical device - fill in the MAC instead of adding a duplicate.
		var mid int64
		err = s.db.QueryRowContext(ctx,
			`SELECT id FROM shelly_devices WHERE address = ? AND mac = '' AND state <> ?`,
			d.Address, StateIgnored).Scan(&mid)
		switch {
		case err == nil:
			_, err = s.db.ExecContext(ctx,
				`UPDATE shelly_devices SET mac = ?, name = ?, model = ?, updated_at = ? WHERE id = ?`,
				d.MAC, d.Name, d.Model, now, mid)
			if err != nil {
				return AdoptSkippedFull, fmt.Errorf("shellystore: adopt upgrade: %w", err)
			}
			return AdoptedKnown, nil
		case !errors.Is(err, sql.ErrNoRows):
			return AdoptSkippedFull, fmt.Errorf("shellystore: adopt addr lookup: %w", err)
		}
	} else {
		// Unknown MAC: dedupe by address among the non-ignored rows.
		var id int64
		err := s.db.QueryRowContext(ctx,
			`SELECT id FROM shelly_devices WHERE address = ? AND state <> ?`, d.Address, StateIgnored).Scan(&id)
		switch {
		case err == nil:
			_, err = s.db.ExecContext(ctx,
				`UPDATE shelly_devices SET name = ?, model = ?, updated_at = ? WHERE id = ?`,
				d.Name, d.Model, now, id)
			if err != nil {
				return AdoptSkippedFull, fmt.Errorf("shellystore: adopt refresh addr: %w", err)
			}
			return AdoptedKnown, nil
		case !errors.Is(err, sql.ErrNoRows):
			return AdoptSkippedFull, fmt.Errorf("shellystore: adopt addr lookup2: %w", err)
		}
	}

	// 3. Fresh device. The gate decides the target state; each set is capped
	//    independently.
	targetState, result := StatePending, AdoptedPending
	countFn := s.CountPending
	if autoAdopt {
		targetState, result = StateActive, AdoptedNew
		countFn = s.CountActive
	}
	// Drop the stale occupant of this address (a different device that used
	// to hold the IP). A device arriving as ACTIVE claims the address and
	// evicts any prior non-ignored occupant; a device arriving as PENDING
	// only evicts another PENDING occupant - it must never delete an approved
	// (active) device on the strength of an unapproved find. Counted AFTER
	// the cleanup so a replaced device does not consume a slot.
	if err := s.clearAddressForState(ctx, d.Address, targetState, 0); err != nil {
		return AdoptSkippedFull, err
	}
	n, err := countFn(ctx)
	if err != nil {
		return AdoptSkippedFull, err
	}
	if limit > 0 && n >= limit {
		return AdoptSkippedFull, nil
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO shelly_devices (mac, address, origin, state, name, model, first_seen_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		d.MAC, d.Address, OriginDiscovered, targetState, d.Name, d.Model, now, now)
	if err != nil {
		return AdoptSkippedFull, fmt.Errorf("shellystore: adopt insert: %w", err)
	}
	return result, nil
}

// ListPending returns the devices found by discovery that await approval,
// most recently seen first. These are records ONLY - never polled.
func (s *Store) ListPending(ctx context.Context) ([]Device, error) {
	return s.query(ctx, `WHERE state = ? ORDER BY updated_at DESC, id`, StatePending)
}

// CountPending returns how many devices await approval (for the pending cap).
func (s *Store) CountPending(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM shelly_devices WHERE state = ?`, StatePending).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("shellystore: count pending: %w", err)
	}
	return n, nil
}

// ApprovePending activates a pending device (it joins the fleet and is
// polled). It honours the active cap (limit; 0 disables) so approvals cannot
// push the polled set past it - ErrAtCap when full. Before the flip it drops
// any stale active row at the same address so approval cannot create a
// duplicate active address. Returns ErrNotFound when the id is not a pending
// row.
func (s *Store) ApprovePending(ctx context.Context, id int64, limit int) error {
	now := s.now().UnixMilli()
	var address string
	err := s.db.QueryRowContext(ctx,
		`SELECT address FROM shelly_devices WHERE id = ? AND state = ?`, id, StatePending).Scan(&address)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("shellystore: approve lookup: %w", err)
	}
	if limit > 0 {
		// Count active devices NOT at this address (this approval replaces
		// any active row already there, so it does not add a net slot).
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM shelly_devices WHERE state = ? AND address <> ?`,
			StateActive, address).Scan(&n); err != nil {
			return fmt.Errorf("shellystore: approve count: %w", err)
		}
		if n >= limit {
			return ErrAtCap
		}
	}
	if err := s.clearAddressExcept(ctx, address, id); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE shelly_devices SET state = ?, updated_at = ? WHERE id = ? AND state = ?`,
		StateActive, now, id, StatePending)
	if err != nil {
		return fmt.Errorf("shellystore: approve: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RejectPending sends a pending device to the ignore list (sticky, keeping
// its MAC + address) so discovery does not surface it again. Returns
// ErrNotFound when the id is not a pending row.
func (s *Store) RejectPending(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE shelly_devices SET state = ?, updated_at = ? WHERE id = ? AND state = ?`,
		StateIgnored, s.now().UnixMilli(), id, StatePending)
	if err != nil {
		return fmt.Errorf("shellystore: reject: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// clearAddressExcept deletes every NON-ignored row (active or pending) at
// address other than exceptID (pass 0 to delete all). Used when an ACTIVE
// device claims the address (a fresh auto-adopt, an active device's move, or
// an approval): it evicts the IP's stale prior occupant. Ignored rows are
// never touched, so the sticky list is preserved.
func (s *Store) clearAddressExcept(ctx context.Context, address string, exceptID int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM shelly_devices WHERE address = ? AND state <> ? AND id <> ?`,
		address, StateIgnored, exceptID)
	if err != nil {
		return fmt.Errorf("shellystore: clear address: %w", err)
	}
	return nil
}

// clearPendingAddressExcept deletes only PENDING rows at address other than
// exceptID. Used when a device arrives/moves as PENDING: it may supersede
// another unapproved candidate at the same IP, but must NOT delete an
// approved (active) device that happens to share the address - approval is
// the operator's, not discovery's, to revoke.
func (s *Store) clearPendingAddressExcept(ctx context.Context, address string, exceptID int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM shelly_devices WHERE address = ? AND state = ? AND id <> ?`,
		address, StatePending, exceptID)
	if err != nil {
		return fmt.Errorf("shellystore: clear pending address: %w", err)
	}
	return nil
}

// clearAddressForState dispatches to the right cleanup for a device arriving
// at address in targetState: an ACTIVE arrival claims the IP (evicts any
// non-ignored occupant); a PENDING arrival only supersedes another pending
// candidate (leaving an approved active device intact).
func (s *Store) clearAddressForState(ctx context.Context, address, targetState string, exceptID int64) error {
	if targetState == StateActive {
		return s.clearAddressExcept(ctx, address, exceptID)
	}
	return s.clearPendingAddressExcept(ctx, address, exceptID)
}

// isIgnored reports whether a device (mac when non-empty, plus address) is
// on the ignore list. Matching is durable on MAC and falls back to address:
//
//   - a MAC on the ignore list always matches (sticky across a DHCP address
//     change - the device stays gone wherever it moves);
//   - an address on the ignore list matches ONLY when it is not contradicted
//     by a different, known MAC. So an ignored row {MX, A} does NOT block a
//     genuinely different device {MY, A} that later inherited A's IP (MY was
//     never removed). An address-only ignore (the ignored row's MAC is
//     unknown) still blocks by address - the conservative choice for a
//     removed device we could not fingerprint.
func (s *Store) isIgnored(ctx context.Context, mac, address string) (bool, error) {
	exists := func(query string, args ...any) (bool, error) {
		var one int
		err := s.db.QueryRowContext(ctx, query, args...).Scan(&one)
		if err == nil {
			return true, nil
		}
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("shellystore: ignore lookup: %w", err)
	}
	if mac != "" {
		if ok, err := exists(
			`SELECT 1 FROM shelly_devices WHERE state = ? AND mac = ? LIMIT 1`,
			StateIgnored, mac); ok || err != nil {
			return ok, err
		}
		// Address match, but not when the ignored row names a DIFFERENT MAC.
		return exists(
			`SELECT 1 FROM shelly_devices WHERE state = ? AND address = ? AND (mac = '' OR mac = ?) LIMIT 1`,
			StateIgnored, address, mac)
	}
	return exists(
		`SELECT 1 FROM shelly_devices WHERE state = ? AND address = ? LIMIT 1`,
		StateIgnored, address)
}

// RemoveByAddress sticky-removes the active device at address: it flips to
// the ignore list, keeping its MAC and address so discovery skips it on the
// next announcement. Returns ErrNotFound when no active row matches.
func (s *Store) RemoveByAddress(ctx context.Context, address string) error {
	now := s.now().UnixMilli()
	res, err := s.db.ExecContext(ctx,
		`UPDATE shelly_devices SET state = ?, updated_at = ? WHERE address = ? AND state = ?`,
		StateIgnored, now, address, StateActive)
	if err != nil {
		return fmt.Errorf("shellystore: remove: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ReleaseByID removes an ignored device from the ignore list entirely, so a
// future announcement can adopt it fresh again. Only ignored rows may be
// released (an active row is not on the list). Returns ErrNotFound
// otherwise.
func (s *Store) ReleaseByID(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM shelly_devices WHERE id = ? AND state = ?`, id, StateIgnored)
	if err != nil {
		return fmt.Errorf("shellystore: release: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ReplaceManual reconciles the active, manually-configured rows to exactly
// the given canonical addresses (the settings IP list is authoritative for
// manual pins). Addresses not yet present are added as manual/active;
// manual/active rows whose address is no longer listed are deleted.
// Discovered and ignored rows are never touched here - the sticky ignore
// list and mDNS finds are independent of the manual list.
//
// An address that currently exists only as a NON-manual active row (already
// discovered) is left as-is: it is already in the set, and duplicating it
// as manual would violate the address dedupe. An address that is on the
// ignore list is NOT re-added - typing back the SAME address a device was
// removed at does not defeat the sticky removal (Release is the way back).
// The one case this cannot catch is a device that has since DHCP-moved to a
// NEW address: typing that new address is treated as a deliberate manual
// pin (we have no MAC for a bare typed address to tie it to the ignore
// entry). The durable defence stays on the discovery path, which ignores by
// MAC wherever the device moves.
func (s *Store) ReplaceManual(ctx context.Context, addresses []string) error {
	now := s.now().UnixMilli()
	want := make(map[string]bool, len(addresses))
	for _, a := range addresses {
		a = strings.TrimSpace(a)
		if a != "" {
			want[a] = true
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("shellystore: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Existing active rows, by address, so we can tell manual-present from
	// discovered-present from absent.
	rows, err := tx.QueryContext(ctx,
		`SELECT id, address, origin FROM shelly_devices WHERE state = ?`, StateActive)
	if err != nil {
		return fmt.Errorf("shellystore: replace scan: %w", err)
	}
	type row struct {
		id     int64
		addr   string
		origin string
	}
	var existing []row
	presentAddr := make(map[string]bool)
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.addr, &r.origin); err != nil {
			rows.Close()
			return fmt.Errorf("shellystore: replace scan row: %w", err)
		}
		existing = append(existing, r)
		presentAddr[r.addr] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("shellystore: replace rows: %w", err)
	}
	rows.Close()

	// Delete manual rows no longer wanted.
	for _, r := range existing {
		if r.origin == OriginManual && !want[r.addr] {
			if _, err := tx.ExecContext(ctx, `DELETE FROM shelly_devices WHERE id = ?`, r.id); err != nil {
				return fmt.Errorf("shellystore: replace delete: %w", err)
			}
		}
	}

	// Insert wanted addresses that are not present as an active row yet. An
	// address on the ignore list is skipped (Release is the way back), so a
	// manual re-add cannot silently defeat a sticky removal.
	for addr := range want {
		if presentAddr[addr] {
			continue
		}
		var one int
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM shelly_devices WHERE address = ? AND state = ? LIMIT 1`,
			addr, StateIgnored).Scan(&one)
		if err == nil {
			continue // ignored at this address - do not resurrect via the manual list
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("shellystore: replace ignore check: %w", err)
		}
		// A pending find at this address is superseded by the deliberate
		// manual pin (the operator typing the IP is an explicit approval);
		// drop it so the address does not end up both pending and active.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM shelly_devices WHERE address = ? AND state = ?`, addr, StatePending); err != nil {
			return fmt.Errorf("shellystore: replace clear pending: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO shelly_devices (mac, address, origin, state, name, model, first_seen_at, updated_at)
			 VALUES ('', ?, ?, ?, '', '', ?, ?)`,
			addr, OriginManual, StateActive, now, now); err != nil {
			return fmt.Errorf("shellystore: replace insert: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("shellystore: replace commit: %w", err)
	}
	return nil
}
