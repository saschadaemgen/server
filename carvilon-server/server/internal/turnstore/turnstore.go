// Package turnstore is the edge-side persistence and live-snapshot
// layer for the TURN/STUN/ICE admin menu (Saison 18-10).
//
// Topology (Weg A): the TURN relay runs on the VPS (cloud role), but
// the admin UI and SQLite live on the RPi (edge role). The cloud
// forwards telemetry over the mTLS side-channel; the edge persists it
// here and serves /a/turn. The whipclient's own ICE-state events are
// edge-local and land here directly.
//
// Two hard rules, both Sascha decisions:
//
//   - PRIVACY: only the MASKED address is ever stored. The raw client
//     IP is dropped on the VPS before a turnstore.Event is built, so it
//     never crosses the network, never reaches the edge, never touches
//     SQLite. The types here have no raw-IP field at all.
//   - OPEN-CORE: this package is carvilon-owned and imports no pion and
//     no carvilon.local/stream. The tagged cloud/edge closures convert
//     the stream layer's types into these. The public build links this
//     package without pulling either dependency.
package turnstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Event is one TURN lifecycle/auth event for the history list. Carries
// only the masked address (never the raw IP), the ephemeral REST
// username (public, not a secret), and never the TURN shared secret or
// the credential password.
type Event struct {
	// Kind is one of "allocation_created", "allocation_deleted",
	// "allocation_error", "auth".
	Kind string `json:"kind"`
	// Time is the VPS-side event time (informational; the freshness
	// clock is the edge receive time, not this).
	Time time.Time `json:"time"`

	SrcMasked string `json:"src_masked,omitempty"`
	DstMasked string `json:"dst_masked,omitempty"`
	Protocol  string `json:"protocol,omitempty"`
	Username  string `json:"username,omitempty"`
	Realm     string `json:"realm,omitempty"`
	// AuthOK is set only for Kind=="auth"; nil otherwise.
	AuthOK *bool `json:"auth_ok,omitempty"`
	// Err is set only for "allocation_error".
	Err string `json:"err,omitempty"`
}

// ICEEvent is one ICE connection-state transition observed by the
// edge whipclient during a cloud publish. Edge-local: it never crosses
// the side-channel.
type ICEEvent struct {
	StreamID     string    `json:"stream_id"`
	State        string    `json:"state"`
	Time         time.Time `json:"time"`
	SinceStartMS int64     `json:"since_start_ms"`
}

// Client is one active relay allocation in a live snapshot. Masked
// address only; no secret/password.
type Client struct {
	SrcMasked string    `json:"src_masked"`
	Username  string    `json:"username,omitempty"`
	Since     time.Time `json:"since"`
}

// Snapshot is the periodic point-in-time view the cloud pushes to the
// edge for the live-stats panel. It bundles the relay's live numbers
// (Enabled/AllocationCount/Clients) with a static config view
// (UDPPort/.../CertMode) the cloud reads from its own config, so the
// edge can render the whole page without holding any TURN config
// itself. GeneratedAt is when the VPS built it; the freshness clock on
// the edge is the receive time, kept by SnapshotHolder.
type Snapshot struct {
	Enabled         bool      `json:"enabled"`
	AllocationCount int       `json:"allocation_count"`
	Clients         []Client  `json:"clients,omitempty"`
	GeneratedAt     time.Time `json:"generated_at"`

	UDPPort        int    `json:"udp_port,omitempty"`
	TLSPort        int    `json:"tls_port,omitempty"`
	Realm          string `json:"realm,omitempty"`
	TURNSHost      string `json:"turns_host,omitempty"`
	STUNActive     bool   `json:"stun_active"`
	CredTTLSeconds int    `json:"cred_ttl_seconds,omitempty"`
	// CertMode is "separate" when the turns: leg uses its own
	// public cert, "shared" when it falls back to the WHIP cert, or
	// "" when the TLS leg is off.
	CertMode string `json:"cert_mode,omitempty"`
}

// Store persists turn_events and ice_state_events in the carvilon
// SQLite DB (Migration 019). Timestamps are stored as unix
// milliseconds, matching the schema_version convention.
type Store struct {
	db *sql.DB
}

// NewStore wraps an open *sql.DB. The caller owns the handle's
// lifecycle (it is the shared carvilon DB).
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// InsertEvent appends one TURN event. Empty optional fields are stored
// as SQL NULL and read back as "".
func (s *Store) InsertEvent(ctx context.Context, e Event) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO turn_events
		   (kind, ts, src_masked, dst_masked, protocol, username, realm, auth_ok, err)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Kind, e.Time.UnixMilli(),
		nullStr(e.SrcMasked), nullStr(e.DstMasked), nullStr(e.Protocol),
		nullStr(e.Username), nullStr(e.Realm), nullBool(e.AuthOK), nullStr(e.Err),
	)
	if err != nil {
		return fmt.Errorf("turnstore: insert event: %w", err)
	}
	return nil
}

// InsertICEEvent appends one ICE-state transition.
func (s *Store) InsertICEEvent(ctx context.Context, e ICEEvent) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO ice_state_events (stream_id, state, ts, since_start_ms)
		 VALUES (?, ?, ?, ?)`,
		e.StreamID, e.State, e.Time.UnixMilli(), e.SinceStartMS,
	)
	if err != nil {
		return fmt.Errorf("turnstore: insert ice event: %w", err)
	}
	return nil
}

// RecentEvents returns the newest `limit` TURN events, newest first.
func (s *Store) RecentEvents(ctx context.Context, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, ts, src_masked, dst_masked, protocol, username, realm, auth_ok, err
		   FROM turn_events
		  ORDER BY ts DESC, id DESC
		  LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("turnstore: recent events: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var (
			e        Event
			ts       int64
			src, dst sql.NullString
			proto    sql.NullString
			user     sql.NullString
			realm    sql.NullString
			authOK   sql.NullBool
			errStr   sql.NullString
		)
		if err := rows.Scan(&e.Kind, &ts, &src, &dst, &proto, &user, &realm, &authOK, &errStr); err != nil {
			return nil, fmt.Errorf("turnstore: scan event: %w", err)
		}
		e.Time = time.UnixMilli(ts)
		e.SrcMasked = src.String
		e.DstMasked = dst.String
		e.Protocol = proto.String
		e.Username = user.String
		e.Realm = realm.String
		e.Err = errStr.String
		if authOK.Valid {
			v := authOK.Bool
			e.AuthOK = &v
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecentICEEvents returns the newest `limit` ICE-state events, newest
// first.
func (s *Store) RecentICEEvents(ctx context.Context, limit int) ([]ICEEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT stream_id, state, ts, since_start_ms
		   FROM ice_state_events
		  ORDER BY ts DESC, id DESC
		  LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("turnstore: recent ice events: %w", err)
	}
	defer rows.Close()

	var out []ICEEvent
	for rows.Next() {
		var (
			e  ICEEvent
			ts int64
		)
		if err := rows.Scan(&e.StreamID, &e.State, &ts, &e.SinceStartMS); err != nil {
			return nil, fmt.Errorf("turnstore: scan ice event: %w", err)
		}
		e.Time = time.UnixMilli(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Purge deletes every event older than `before` from both tables and
// returns the total number of rows removed. This is the 30-day
// retention sweep (Sascha decision).
func (s *Store) Purge(ctx context.Context, before time.Time) (int64, error) {
	cutoff := before.UnixMilli()
	var total int64
	for _, table := range []string{"turn_events", "ice_state_events"} {
		res, err := s.db.ExecContext(ctx, "DELETE FROM "+table+" WHERE ts < ?", cutoff)
		if err != nil {
			return total, fmt.Errorf("turnstore: purge %s: %w", table, err)
		}
		if n, err := res.RowsAffected(); err == nil {
			total += n
		}
	}
	return total, nil
}

// nullStr maps "" to SQL NULL so an absent optional field is stored as
// NULL rather than an empty string.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullBool maps a nil *bool to SQL NULL.
func nullBool(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}
