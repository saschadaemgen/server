-- Saison 14-01b: per-viewer idle-view-mode plus station coordinates.
--
-- viewers.idle_view_mode picks which idle UI the mieter browser
-- renders by default:
--   NULL          -> "screensaver" (clock + date + weather)
--   'screensaver' -> same as NULL (explicit choice via settings)
--   'livestream'  -> live MJPEG img
-- Tap on the idle container toggles temporarily; reload returns
-- to the persisted default.
--
-- platform_config gets two new well-known keys: station_lat and
-- station_lon. These point the open-meteo client at the operator's
-- physical site. Defaults are Recklinghausen (the saison-1-13 test
-- site); the admin overrides them in /a/settings.

ALTER TABLE viewers ADD COLUMN idle_view_mode TEXT;

INSERT INTO platform_config (key, value, updated_at)
VALUES
    ('station_lat', '51.6144', strftime('%s','now')*1000),
    ('station_lon', '7.1959',  strftime('%s','now')*1000)
ON CONFLICT (key) DO NOTHING;

INSERT INTO schema_version (version, applied_at)
VALUES (13, strftime('%s','now')*1000);
