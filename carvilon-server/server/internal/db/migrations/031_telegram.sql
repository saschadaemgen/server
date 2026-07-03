-- Telegram-Track: Chat-Allowlist + wartende Chats.
--
-- Der Telegram-Bot (internal/telegrambot) ist die erste, bewusst
-- eingezaeunte Cloud-Funktion: Ausgang nur zu api.telegram.org,
-- default-deny fuer Chats. Nur Chat-IDs in telegram_allowed_chats
-- duerfen Befehle ausloesen und Nachrichten empfangen. Der Bot-Token
-- liegt NICHT hier, sondern AES-256-GCM-verschluesselt in
-- platform_config (KeyTelegramBotToken).
--
--   telegram_allowed_chats - die Allowlist. chat_id ist Telegrams
--       64-Bit-Chat-ID (negativ fuer Gruppen). label ist die
--       menschenlesbare Bezeichnung fuer Admin-Seite und Editor-Picker.
--
--   telegram_pending_chats - unbekannte Chats, die dem Bot geschrieben
--       haben und auf Freigabe warten (Chat-ID-Ermittlung im Produkt,
--       Muster esp_pending_devices). rejected_at bleibt gesetzt, damit
--       ein abgelehnter Chat nicht bei der naechsten Nachricht wieder
--       als "wartend" auftaucht. username/first_name sind reine
--       Anzeige-Metadaten aus der Nachricht.

CREATE TABLE telegram_allowed_chats (
    chat_id    INTEGER PRIMARY KEY,
    label      TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE TABLE telegram_pending_chats (
    chat_id     INTEGER PRIMARY KEY,
    username    TEXT,
    first_name  TEXT,
    first_seen  INTEGER NOT NULL,
    last_seen   INTEGER NOT NULL,
    rejected_at INTEGER
);

INSERT INTO schema_version (version, applied_at)
VALUES (31, strftime('%s','now')*1000);
