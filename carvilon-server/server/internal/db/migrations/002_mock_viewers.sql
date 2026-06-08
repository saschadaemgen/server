CREATE TABLE mock_viewers (
    mac           TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    service_port  INTEGER NOT NULL,
    ua_user_id    TEXT,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);

CREATE INDEX idx_mock_viewers_ua_user ON mock_viewers(ua_user_id);
CREATE UNIQUE INDEX idx_mock_viewers_port ON mock_viewers(service_port);

INSERT INTO schema_version (version, applied_at) VALUES (2, strftime('%s','now')*1000);
