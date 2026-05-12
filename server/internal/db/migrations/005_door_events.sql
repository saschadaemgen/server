-- Saison 13-01: Doorbell-History.
--
-- door_events ist der zentrale Audit-Trail fuer Klingel-Lifecycle-
-- Eintraege. Der doorbellhub schreibt eingehende Frames parallel
-- zur SSE-Distribution hierhin; das Mieter-UI rendert die Liste
-- plus einen Ungelesen-Badge; das Admin-Dashboard aggregiert.
--
-- Saison 14 dockt einen UA-Webhook-Endpoint an dieselbe Tabelle an
-- (Event-Type-Dispatch); Saison 16+ Stempelkarten-Plugin kann
-- ueber event_type weitere Strings dazumixen (punch_in, punch_out,
-- visitor_enter) und die jetzt schon vorgesehenen Hash-Chain-
-- Spalten prev_hash und entry_hash aktiv schreiben. In Saison 13
-- bleiben diese Felder NULL.

CREATE TABLE door_events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    mock_mac        TEXT NOT NULL,
    event_type      TEXT NOT NULL,
    intercom_mac    TEXT,
    occurred_at     INTEGER NOT NULL,
    cancelled_at    INTEGER,
    answered_at     INTEGER,
    ended_at        INTEGER,
    cancel_token    TEXT,
    room_id         TEXT,
    read_at         INTEGER,
    prev_hash       TEXT,
    entry_hash      TEXT,
    raw_frame       BLOB,
    FOREIGN KEY (mock_mac) REFERENCES mock_viewers(mac) ON DELETE CASCADE
);

CREATE INDEX idx_door_events_mock_mac_occurred
    ON door_events(mock_mac, occurred_at DESC);

CREATE INDEX idx_door_events_unread
    ON door_events(mock_mac) WHERE read_at IS NULL;

CREATE INDEX idx_door_events_occurred_at
    ON door_events(occurred_at DESC);

INSERT INTO schema_version (version, applied_at) VALUES (5, strftime('%s','now')*1000);
