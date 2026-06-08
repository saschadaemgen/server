CREATE TABLE schema_version (
    version    INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
);

CREATE TABLE magic_link_tokens (
    token        TEXT PRIMARY KEY,
    ua_user_id   TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    consumed_at  INTEGER
);

CREATE INDEX idx_magic_link_ua_user ON magic_link_tokens(ua_user_id);

CREATE TABLE sessions (
    session_id   TEXT PRIMARY KEY,
    ua_user_id   TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    last_seen    INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    user_agent   TEXT,
    ip           TEXT
);

CREATE INDEX idx_sessions_ua_user ON sessions(ua_user_id);
CREATE INDEX idx_sessions_expires ON sessions(expires_at);

INSERT INTO schema_version (version, applied_at) VALUES (1, strftime('%s','now')*1000);
