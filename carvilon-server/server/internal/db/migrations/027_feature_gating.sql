-- Saison 20: Feature-Gating data layer (DB only, no UI, no stream-server).
--
-- The resolution model (top -> bottom, per function + viewer):
--   1. Function catalog (Go, internal/featuregate) - source of truth: key,
--      type, default value, default-active, default-licensed, and the bridge
--      to the existing typed viewer column.
--   2. license / license_features (this installation, one DB = one customer):
--      which functions are unlocked + the viewer limit. A license_features
--      row OVERRIDES the catalog DefaultLicensed; ROW ABSENCE = catalog default
--      (todays settings stay licensed=true -> zero behaviour break).
--   3. templates / template_features: a named preset, per function active +
--      generic value (TEXT, parsed into the catalog type). Templates are LIVE -
--      no copy on attach; a change re-resolves on the next config fetch.
--   4. viewer_feature_active: the per-viewer ACTIVE override (top of the active
--      axis). Mirrors viewer_setting_visibility's shape; row absence = inherit.
--
-- The VALUE keeps living in the existing typed viewers.* columns with the
-- proven NULL-inherits logic (keep_stream); this migration adds NO generic
-- value store for viewer values. "Profil" stays reserved for the stream
-- profile - these are license / template / feature, never "profile".

-- 1) Lizenz/Abo = lokaler Einzeldatensatz (Singleton, id=1). The central
--    manufacturer administration later writes onto exactly this record.
CREATE TABLE license (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    plan_name    TEXT    NOT NULL DEFAULT '',   -- UI says "Abo"
    viewer_limit INTEGER,                        -- NULL = unlimited
    valid_until  INTEGER,                        -- unix millis, NULL = perpetual
    updated_at   INTEGER NOT NULL
);

-- 2) Which functions are unlocked. A row overrides the catalog DefaultLicensed;
--    absence falls back to the catalog default.
CREATE TABLE license_features (
    feature_key TEXT    NOT NULL,                -- free-text, points at a catalog key
    licensed    INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (feature_key)
);

-- 3) Named preset (live, no copy on attach).
CREATE TABLE templates (
    id         INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- 4) Per template + function: active override + generic value (TEXT, parsed
--    per the catalog type). active NULL = inherit catalog DefaultActive;
--    value NULL = no value (inherit viewer-column / catalog default).
CREATE TABLE template_features (
    template_id INTEGER NOT NULL REFERENCES templates(id) ON DELETE CASCADE,
    feature_key TEXT    NOT NULL,
    active      INTEGER,
    value       TEXT,
    PRIMARY KEY (template_id, feature_key)
);
CREATE INDEX idx_template_features_template ON template_features(template_id);

-- 5) Per-viewer ACTIVE override (orthogonal to viewer_setting_visibility's
--    "tenant may see/change the control" - this is "the function is on").
--    Same join-table shape as viewer_doors / viewer_setting_visibility; row
--    absence = inherit (template ?? catalog).
CREATE TABLE viewer_feature_active (
    viewer_mac  TEXT    NOT NULL REFERENCES viewers(mac) ON DELETE CASCADE,
    feature_key TEXT    NOT NULL,
    active      INTEGER NOT NULL,                -- explicit 0/1
    PRIMARY KEY (viewer_mac, feature_key)
);
CREATE INDEX idx_viewer_feature_active_mac ON viewer_feature_active(viewer_mac);

-- 6) Viewer -> template membership (live; NULL = no template). ON DELETE SET
--    NULL is valid for ADD COLUMN because the column default is NULL.
ALTER TABLE viewers ADD COLUMN template_id INTEGER REFERENCES templates(id) ON DELETE SET NULL;

INSERT INTO schema_version (version, applied_at)
VALUES (27, strftime('%s','now')*1000);
