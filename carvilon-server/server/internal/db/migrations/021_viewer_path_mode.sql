-- Saison 19-39: per-viewer transport-path override (the WEG-Schalter).
--
-- 'auto'  = today's behaviour (the app races edge_whep_url / ICE picks the
--           most direct path; LAN-first, cloud as fallback)
-- 'local' = edge only
-- 'cloud' = cloud only
--
-- carvilon only STORES and serves this flag (in settings.json); the app
-- honours it when choosing the endpoint - the transport mechanic itself is
-- NOT touched here (App/Stream). v1 is admin-set; a later tenant override
-- rides the viewer_setting_visibility table (setting_key='path_mode',
-- migration 022). NOT NULL DEFAULT 'auto' -> existing + new rows are 'auto',
-- no backfill needed.

ALTER TABLE viewers ADD COLUMN path_mode TEXT NOT NULL DEFAULT 'auto';

INSERT INTO schema_version (version, applied_at)
VALUES (21, strftime('%s','now')*1000);
