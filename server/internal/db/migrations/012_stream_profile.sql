-- Saison 14-01: per-viewer go2rtc stream profile.
--
-- A viewer can either pin a specific go2rtc profile name (set via
-- the admin /a/streams UI or the viewer edit modal) or leave the
-- column NULL and let the server fall back to a convention:
--   type='esp' -> "intercom_esp"
--   type='web' -> "intercom_browser"
--
-- Stored as plain TEXT, nullable. Existing rows get NULL on the
-- ALTER, which means convention-fallback for all viewers until the
-- admin overrides them.

ALTER TABLE viewers ADD COLUMN stream_profile TEXT;

INSERT INTO schema_version (version, applied_at)
VALUES (12, strftime('%s','now')*1000);
