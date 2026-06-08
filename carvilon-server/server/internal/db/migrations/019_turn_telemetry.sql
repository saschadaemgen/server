-- Saison 18-10: TURN/STUN/ICE telemetry persistence (edge side).
--
-- The TURN relay runs on the VPS (cloud role); it forwards lifecycle
-- events over the mTLS side-channel and the edge (RPi) persists them
-- here for the /a/turn admin menu. The whipclient's own ICE-state
-- transitions are edge-local and land in ice_state_events directly.
--
-- PRIVACY (Sascha decision): only the MASKED address is ever stored.
-- The raw client IP is dropped on the VPS before anything is sent, so
-- it never crosses the network and there is deliberately NO raw-IP
-- column here. Retention is 30 days, swept by internal/turnstore.
--
-- These tables reference nothing (the telemetry is global to the relay,
-- not tied to a viewer row), so no FK and no table-rebuild: a plain
-- CREATE under the schema_version gate (runs only when current < 19).
--
-- Timestamps are unix milliseconds, matching the schema_version
-- convention (strftime('%s','now')*1000).

CREATE TABLE turn_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    kind        TEXT    NOT NULL,   -- allocation_created|allocation_deleted|allocation_error|auth
    ts          INTEGER NOT NULL,   -- unix ms (VPS event time)
    src_masked  TEXT,               -- masked client address (NULL if absent)
    dst_masked  TEXT,               -- masked relay address (only allocation_created)
    protocol    TEXT,               -- udp|tcp
    username    TEXT,               -- ephemeral REST username (NOT a secret)
    realm       TEXT,
    auth_ok     INTEGER,            -- 0/1, NULL unless kind='auth'
    err         TEXT                -- only allocation_error
);
CREATE INDEX idx_turn_events_ts ON turn_events(ts);

CREATE TABLE ice_state_events (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    stream_id      TEXT    NOT NULL,
    state          TEXT    NOT NULL,
    ts             INTEGER NOT NULL,   -- unix ms (edge observe time)
    since_start_ms INTEGER NOT NULL
);
CREATE INDEX idx_ice_state_events_ts ON ice_state_events(ts);

INSERT INTO schema_version (version, applied_at)
VALUES (19, strftime('%s','now')*1000);
