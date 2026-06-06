-- Saison 19-39: per-(viewer, setting) visibility for the tenant app.
--
-- visible_to_tenant=0 makes a setting admin-only (the app hides the
-- mieter's control); ABSENCE of a row = visible (default), so no backfill
-- is needed. The stored VALUE still governs behaviour regardless of
-- visibility - this table only gates whether the tenant sees/changes the
-- control. Foundation for later premium tiers; it also generalises the
-- later path_mode tenant-override (setting_key='path_mode').
--
-- setting_key is FREE-TEXT (not an enum) so new keys need no schema change.
-- Exact viewer_doors pattern: FK to viewers(mac) ON DELETE CASCADE
-- (foreign_keys=ON in production; the migration loop runs with FK off and
-- foreign_key_check passes - the table is empty).

CREATE TABLE viewer_setting_visibility (
    viewer_mac        TEXT    NOT NULL REFERENCES viewers(mac) ON DELETE CASCADE,
    setting_key       TEXT    NOT NULL,
    visible_to_tenant INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (viewer_mac, setting_key)
);
CREATE INDEX idx_viewer_setting_visibility_mac ON viewer_setting_visibility(viewer_mac);

INSERT INTO schema_version (version, applied_at)
VALUES (22, strftime('%s','now')*1000);
