-- Terminal-Track Schritt 1: gespeicherte Verbindungs-Profile des
-- Konsolen-Docks plus die TOFU-Ablage der Host-Schluessel.
--
--   console_profiles - ein Schnell-Verbinden-Profil (Name, Host, Port,
--       Benutzer, Anmelde-Art). Die Geheimnisse liegen NIE im Klartext:
--       secret_enc traegt das AES-256-GCM-Chiffrat (Passwort ODER
--       privater Schluessel, je nach auth_kind), passphrase_enc optional
--       die Schluessel-Passphrase. Beide werden ueber internal/secrets
--       ver- und entschluesselt, nie geloggt, nie zurueckgegeben (die
--       API meldet nur "gesetzt"/"nicht gesetzt", Muster Bot-Token).
--
--   console_host_keys - Trust-on-first-use. Beim ersten Verbinden wird
--       der Fingerprint des Server-Host-Schluessels gepinnt; aendert er
--       sich spaeter, wird die Verbindung geblockt, bis ein Admin ihn
--       ausdruecklich neu vertraut (Zeile ersetzt). host_port ist der
--       Schluessel, damit derselbe Host auf zwei Ports getrennt pinnt.

CREATE TABLE console_profiles (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    name           TEXT NOT NULL,
    host           TEXT NOT NULL,
    port           INTEGER NOT NULL DEFAULT 22,
    username       TEXT NOT NULL,
    auth_kind      TEXT NOT NULL CHECK (auth_kind IN ('password', 'key')),
    secret_enc     TEXT NOT NULL DEFAULT '',
    passphrase_enc TEXT NOT NULL DEFAULT '',
    sort           INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);

CREATE TABLE console_host_keys (
    host_port   TEXT PRIMARY KEY,
    key_type    TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    added_at    INTEGER NOT NULL
);

INSERT INTO schema_version (version, applied_at)
VALUES (33, strftime('%s','now')*1000);
