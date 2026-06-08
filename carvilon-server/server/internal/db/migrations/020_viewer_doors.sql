-- Saison 19-30: 1:n door assignment (viewer -> multiple UA doors).
--
-- Replaces the single-bell paired_intercom_mac model in the admin UI
-- with an explicit per-viewer door list. paired_intercom_mac STAYS
-- (legacy + in-call auto-resolution fallback); this table is purely
-- additive.
--
-- door_id is a UA-Access door UUID (GET /api/v1/developer/doors). The
-- mieter-unlock path authorises a direct-UUID unlock against THIS
-- table, so a logged-in viewer can only open doors an admin assigned.
-- label/sort are UI conveniences (display name override + ordering).
--
-- FK to viewers(mac) ON DELETE CASCADE removes the assignments when a
-- viewer is deleted (foreign_keys=ON in production; the migration loop
-- runs with FK off and foreign_key_check passes because the table is
-- empty). No timestamps: this is configuration, not an event log.

CREATE TABLE viewer_doors (
    viewer_mac TEXT    NOT NULL REFERENCES viewers(mac) ON DELETE CASCADE,
    door_id    TEXT    NOT NULL,   -- UA-Access door UUID
    label      TEXT    NOT NULL DEFAULT '',
    sort       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (viewer_mac, door_id)
);
CREATE INDEX idx_viewer_doors_mac ON viewer_doors(viewer_mac);

INSERT INTO schema_version (version, applied_at)
VALUES (20, strftime('%s','now')*1000);
