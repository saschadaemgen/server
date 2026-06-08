-- Saison 13-02-FIX4-a: Viewer-Auth + Vokabular + Security-Hardening.
--
-- Migrationsstrategie: ALTER TABLE RENAME (SQLite >= 3.25.0
-- aktualisiert FK-Klauseln in abhaengigen Tabellen automatisch).
-- So bleiben Daten und Indizes intakt; PRAGMA foreign_keys muss
-- nicht waehrend der Migration umgeschaltet werden (was im
-- Single-Tx-Migrationsrunner ohnehin No-Op waere).
--
-- 1) mock_viewers       -> viewers, plus type-Spalte und Auth-Felder
-- 2) mieter_sessions    -> viewer_sessions, mock_mac -> viewer_mac
-- 3) door_events.mock_mac -> viewer_mac
-- 4) magic_link_tokens  -> RAUS (kein Magic-Link mehr)
-- 5) login_audit        -> NEU (eine Zeile pro Anmeldeversuch)

ALTER TABLE mock_viewers RENAME TO viewers;

ALTER TABLE viewers ADD COLUMN type TEXT NOT NULL DEFAULT 'web'
    CHECK(type IN ('web', 'esp'));
ALTER TABLE viewers ADD COLUMN username           TEXT;
ALTER TABLE viewers ADD COLUMN password_hash      TEXT;
ALTER TABLE viewers ADD COLUMN password_set_at    INTEGER;
ALTER TABLE viewers ADD COLUMN esp_token_hash     TEXT;
ALTER TABLE viewers ADD COLUMN esp_device_id      TEXT;
ALTER TABLE viewers ADD COLUMN esp_pending        INTEGER NOT NULL DEFAULT 0;
ALTER TABLE viewers ADD COLUMN esp_model          TEXT;
ALTER TABLE viewers ADD COLUMN esp_fw_version     TEXT;
ALTER TABLE viewers ADD COLUMN linked_ua_user_id  TEXT;

UPDATE viewers SET username = name WHERE username IS NULL;

DROP INDEX IF EXISTS idx_mock_viewers_port;
CREATE UNIQUE INDEX idx_viewers_service_port ON viewers(service_port);
CREATE UNIQUE INDEX idx_viewers_username
    ON viewers(username) WHERE username IS NOT NULL;
CREATE INDEX idx_viewers_type ON viewers(type);

ALTER TABLE mieter_sessions RENAME TO viewer_sessions;
ALTER TABLE viewer_sessions RENAME COLUMN mock_mac TO viewer_mac;

DROP INDEX IF EXISTS idx_mieter_sessions_mock;
DROP INDEX IF EXISTS idx_mieter_sessions_expires;
CREATE INDEX idx_viewer_sessions_mac     ON viewer_sessions(viewer_mac);
CREATE INDEX idx_viewer_sessions_expires ON viewer_sessions(expires_at);

ALTER TABLE door_events RENAME COLUMN mock_mac TO viewer_mac;

DROP INDEX IF EXISTS idx_door_events_mock_mac_occurred;
DROP INDEX IF EXISTS idx_door_events_unread;
CREATE INDEX idx_door_events_viewer_mac_occurred
    ON door_events(viewer_mac, occurred_at DESC);
CREATE INDEX idx_door_events_unread
    ON door_events(viewer_mac) WHERE read_at IS NULL;

DROP TABLE IF EXISTS magic_link_tokens;

CREATE TABLE login_audit (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp    INTEGER NOT NULL,
    realm        TEXT NOT NULL CHECK(realm IN ('viewer', 'admin')),
    username     TEXT,
    viewer_mac   TEXT,
    ip           TEXT,
    user_agent   TEXT,
    outcome      TEXT NOT NULL
                 CHECK(outcome IN ('success','fail','locked','unlocked'))
);

CREATE INDEX idx_login_audit_timestamp ON login_audit(timestamp DESC);
CREATE INDEX idx_login_audit_username  ON login_audit(username);
CREATE INDEX idx_login_audit_ip        ON login_audit(ip);

INSERT INTO schema_version (version, applied_at)
VALUES (6, strftime('%s','now')*1000);
