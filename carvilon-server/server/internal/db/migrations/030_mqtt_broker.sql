-- MQTT-Broker Schritt 1: Geraete-Anmeldedaten + ACL-Regeln.
--
-- Der eingebettete MQTT-Broker (mochi-mqtt) authentifiziert jedes
-- Geraet beim CONNECT gegen mqtt_devices und autorisiert jeden
-- Publish/Subscribe gegen mqtt_acl_rules. Diese Tabellen sind
-- STRIKT getrennt von den menschlichen Admin-Konten (admin_users)
-- und den Viewer-Geraeten (viewers): ein MQTT-Geraet ist eine
-- eigene Identitaet mit eigenem Lebenszyklus.
--
--   password_hash  - Argon2id-PHC-String (m=64MB,t=3,p=4) mit dem
--                    platform-weiten Pepper. NIE Klartext.
--   label          - optionaler menschenlesbarer Name fuer die
--                    Admin-Liste (z.B. "Flur-Taster EG").
--   last_connect_at - zuletzt erfolgreicher CONNECT (ms), NULL bis
--                    zur ersten Verbindung. Reine Anzeige.
--
-- ACL-Modell (Auswertung im Broker):
--   action       - 'publish' | 'subscribe' | 'both'
--   topic_filter - MQTT-Topic-Filter (+ und # erlaubt)
--   allow        - 1 = erlauben, 0 = verbieten (deny gewinnt)
-- Ohne passende Regel greift der Default-deny; ein Geraet darf per
-- Voreinstellung nur seinen eigenen Teilbaum carvilon/<user>/#.

CREATE TABLE mqtt_devices (
    username        TEXT PRIMARY KEY NOT NULL,
    password_hash   TEXT NOT NULL,
    label           TEXT,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    last_connect_at INTEGER
);

CREATE TABLE mqtt_acl_rules (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    username     TEXT NOT NULL,
    action       TEXT NOT NULL CHECK(action IN ('publish','subscribe','both')),
    topic_filter TEXT NOT NULL,
    allow        INTEGER NOT NULL DEFAULT 1 CHECK(allow IN (0,1)),
    created_at   INTEGER NOT NULL,
    FOREIGN KEY (username) REFERENCES mqtt_devices(username) ON DELETE CASCADE
);

CREATE INDEX idx_mqtt_acl_user ON mqtt_acl_rules(username);

INSERT INTO schema_version (version, applied_at)
VALUES (30, strftime('%s','now')*1000);
