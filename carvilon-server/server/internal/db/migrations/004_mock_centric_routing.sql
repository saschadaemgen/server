-- Saison 12-06 refactor: mock-zentriertes Klingel-Routing.
--
-- Vor diesem Refactor war ua_user_id der Routing-Schluessel
-- ueberall. Live-Erkenntnis: ein Mieter kann mehrere Mocks
-- haben, ein Mock mehreren Klingelschildern zugeordnet sein,
-- manche Mocks haben gar keinen UA-User (LEERSTAND,
-- HAUSVERWALTUNG). Mock-MAC ist der einzige Routing-Schluessel.
--
-- magic_link_tokens und sessions binden ab jetzt an Mock-MAC.
-- Admin-Sessions wandern in eine eigene admin_sessions-Tabelle
-- damit die Mieter-sessions-Tabelle einen harten FK auf
-- mock_viewers haben kann; vorher lagen Admin-Sessions in der
-- gemeinsamen sessions-Tabelle mit dem "_admin_<user>"-Prefix
-- als ua_user_id-Surrogat.
--
-- Alte Tokens und Sessions werden verworfen. Mocks bleiben.

-- mock_viewers: ua_user_id-Spalte entfernen. SQLite kann keine
-- Spalten direkt droppen, also Tabelle neu anlegen.
CREATE TABLE mock_viewers_new (
    mac           TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    service_port  INTEGER NOT NULL,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);

INSERT INTO mock_viewers_new (mac, name, service_port, created_at, updated_at)
SELECT mac, name, service_port, created_at, updated_at FROM mock_viewers;

DROP TABLE mock_viewers;
ALTER TABLE mock_viewers_new RENAME TO mock_viewers;

CREATE UNIQUE INDEX idx_mock_viewers_port ON mock_viewers(service_port);

-- magic_link_tokens: ua_user_id -> mock_mac. Token-Inhalt wird
-- nicht migriert (saison-12-Tokens sind Klartext, 7-Tage-TTL,
-- nicht wertvoll genug fuer einen Erhalt-Pfad).
DROP TABLE magic_link_tokens;
CREATE TABLE magic_link_tokens (
    token        TEXT PRIMARY KEY,
    mock_mac     TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    consumed_at  INTEGER,
    FOREIGN KEY (mock_mac) REFERENCES mock_viewers(mac) ON DELETE CASCADE
);

CREATE INDEX idx_magic_link_mock ON magic_link_tokens(mock_mac);

-- Mieter-sessions: aus dem alten sessions-Topf isoliert, mit
-- FK auf mock_viewers.
DROP TABLE sessions;
CREATE TABLE mieter_sessions (
    session_id   TEXT PRIMARY KEY,
    mock_mac     TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    last_seen    INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    user_agent   TEXT,
    ip           TEXT,
    FOREIGN KEY (mock_mac) REFERENCES mock_viewers(mac) ON DELETE CASCADE
);

CREATE INDEX idx_mieter_sessions_mock    ON mieter_sessions(mock_mac);
CREATE INDEX idx_mieter_sessions_expires ON mieter_sessions(expires_at);

-- Admin-sessions: aus dem alten sessions-Topf isoliert, mit FK
-- auf admin_users.
CREATE TABLE admin_sessions (
    session_id      TEXT PRIMARY KEY,
    admin_username  TEXT NOT NULL,
    created_at      INTEGER NOT NULL,
    last_seen       INTEGER NOT NULL,
    expires_at      INTEGER NOT NULL,
    user_agent      TEXT,
    ip              TEXT,
    FOREIGN KEY (admin_username) REFERENCES admin_users(username) ON DELETE CASCADE
);

CREATE INDEX idx_admin_sessions_user    ON admin_sessions(admin_username);
CREATE INDEX idx_admin_sessions_expires ON admin_sessions(expires_at);

INSERT INTO schema_version (version, applied_at) VALUES (4, strftime('%s','now')*1000);
