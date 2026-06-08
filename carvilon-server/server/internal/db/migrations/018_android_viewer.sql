-- Saison 16: Android-Viewer (Etappe 1).
--
-- Drei Aenderungen am viewers-Schema, in EINEM table-rebuild
-- erledigt, weil SQLite einen CHECK-Constraint nicht per ALTER
-- aendern kann:
--
--   1) CHECK(type IN (...)) um 'android' erweitern. Android-
--      Viewer sind ein eigener type, leben neben 'web' und 'esp'.
--      Discovery-/Pending-Flow gilt weiter NUR fuer ESP; ein
--      Android-Geraet wird direkt im Admin angelegt.
--   2) esp_token_hash -> device_token_hash. ESP und Android
--      teilen die Bearer-Token-Mechanik (sha256-Hash speichern,
--      One-Shot-Klartext-Reveal, requireDeviceBearer-Middleware).
--      Der Spaltenname reflektiert das jetzt. Die ESP-
--      spezifischen Spalten (esp_pending, esp_model,
--      esp_fw_version, esp_device_id) bleiben unangetastet,
--      Android nutzt sie nicht.
--   3) Zwei neue Spalten:
--        fcm_token TEXT       Etappe 2: FCM-Push-Adresse pro
--                             Geraet. Jetzt leer angelegt damit
--                             Etappe 2 keine eigene Migration
--                             braucht.
--        device_label TEXT    Geraetename ("Papas Handy"). Hilft
--                             dem Admin mehrere Android-Geraete
--                             derselben Wohnung zu unterscheiden.
--
-- FK-Verkettung: viewer_sessions, door_events und
-- viewer_hidden_events haengen alle per FK auf viewers(mac)
-- (zwei davon mit ON DELETE CASCADE). Der Migrations-Runner
-- setzt PRAGMA foreign_keys = OFF VOR der Transaction (siehe
-- internal/db/migrate.go), damit das DROP TABLE viewers nicht
-- implizit alle Child-Rows wegcascaded. Nach RENAME zeigen alle
-- FK-Texte (REFERENCES viewers(mac)) wieder auf eine
-- existierende Tabelle; der foreign_key_check am Ende des
-- Runner-Loops verifiziert die Konsistenz.
--
-- Indizes nach dem Rebuild:
--   idx_viewers_service_port (UNIQUE)
--   idx_viewers_type
-- idx_viewers_username wurde in Migration 008 zusammen mit der
-- username-Spalte entfernt und kehrt nicht zurueck (der
-- Wohnungs-Name IST seit dem der Login).
--
-- Idempotenz: durch das schema_version-Gate des Runners (laeuft
-- nur wenn current_version < 18). INSERT INTO schema_version am
-- Ende setzt 18.

CREATE TABLE viewers_new (
    mac                       TEXT    PRIMARY KEY,
    name                      TEXT    NOT NULL,
    service_port              INTEGER NOT NULL,
    type                      TEXT    NOT NULL DEFAULT 'web'
                                      CHECK(type IN ('web', 'esp', 'android')),
    password_hash             TEXT,
    password_set_at           INTEGER,
    device_token_hash         TEXT,
    esp_device_id             TEXT,
    esp_pending               INTEGER NOT NULL DEFAULT 0,
    esp_model                 TEXT,
    esp_fw_version            TEXT,
    linked_ua_user_id         TEXT,
    paired_intercom_mac       TEXT    NOT NULL DEFAULT '',
    stream_profile            TEXT,
    idle_view_mode            TEXT,
    auto_screensaver_seconds  INTEGER,
    brightness_idle           INTEGER,
    screen_off_after_sec      INTEGER,
    language                  TEXT,
    history_capture_enabled   INTEGER DEFAULT 1,
    clock_layout              TEXT,
    fcm_token                 TEXT,
    device_label              TEXT,
    created_at                INTEGER NOT NULL,
    updated_at                INTEGER NOT NULL
);

INSERT INTO viewers_new (
    mac, name, service_port, type,
    password_hash, password_set_at,
    device_token_hash,
    esp_device_id, esp_pending, esp_model, esp_fw_version,
    linked_ua_user_id, paired_intercom_mac,
    stream_profile, idle_view_mode, auto_screensaver_seconds,
    brightness_idle, screen_off_after_sec, language,
    history_capture_enabled, clock_layout,
    created_at, updated_at
)
SELECT
    mac, name, service_port, type,
    password_hash, password_set_at,
    esp_token_hash,
    esp_device_id, esp_pending, esp_model, esp_fw_version,
    linked_ua_user_id, paired_intercom_mac,
    stream_profile, idle_view_mode, auto_screensaver_seconds,
    brightness_idle, screen_off_after_sec, language,
    history_capture_enabled, clock_layout,
    created_at, updated_at
FROM viewers;

DROP TABLE viewers;
ALTER TABLE viewers_new RENAME TO viewers;

CREATE UNIQUE INDEX idx_viewers_service_port ON viewers(service_port);
CREATE INDEX idx_viewers_type ON viewers(type);

INSERT INTO schema_version (version, applied_at)
VALUES (18, strftime('%s','now')*1000);
