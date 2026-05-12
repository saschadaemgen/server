-- Saison 12-04: Admin auth + platform configuration.
--
-- admin_users is intentionally tiny: saison 12 supports exactly
-- one admin account (the Hausverwalter). A multi-admin migration
-- will come later if the use case warrants it.
--
-- platform_config carries server-wide settings. Each row sets
-- either value or value_encrypted, never both. Secrets land in
-- value_encrypted as hex(nonce || ciphertext_with_tag) produced
-- by the secrets package (AES-256-GCM).

CREATE TABLE admin_users (
    username       TEXT PRIMARY KEY,
    password_hash  TEXT NOT NULL,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    last_login_at  INTEGER
);

CREATE TABLE platform_config (
    key             TEXT PRIMARY KEY,
    value           TEXT,
    value_encrypted TEXT,
    updated_at      INTEGER NOT NULL
);

INSERT INTO schema_version (version, applied_at) VALUES (3, strftime('%s','now')*1000);
