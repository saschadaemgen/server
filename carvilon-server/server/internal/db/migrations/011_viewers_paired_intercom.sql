-- Saison 13-07: per-viewer paired intercom for the standby
-- "Tuer auf"-Knopf.
--
-- Replaces the saison-13-06 platform_config.viewer_to_door
-- approach. Saison-13-07 lifted door-resolution into the
-- UA-API (Door.IntercomMAC parses extras.door_thumbnail), so
-- the only piece carvilon needs to remember per viewer is which
-- INTERCOM the viewer is paired with - the door follows
-- automatically.
--
-- Stored format: lowercase colon-form MAC
-- ("28:70:4e:31:e2:9c"). Empty string means "no pairing,
-- standby button is inert" (the bell-overlay path still works
-- because it learns the intercom from the active doorbell
-- event).
--
-- The two saison-13-05/06 platform_config rows
-- (intercom_to_door + viewer_to_door) become dead weight after
-- this migration - they are not yet deleted by SQL because
-- platform_config is generic and unused rows do no harm; the
-- saison-13-07 housekeeping commit drops them via DELETE FROM
-- platform_config WHERE key IN (...) at server boot when the
-- next major release ships.

ALTER TABLE viewers
    ADD COLUMN paired_intercom_mac TEXT NOT NULL DEFAULT '';

INSERT INTO schema_version (version, applied_at)
VALUES (11, strftime('%s','now')*1000);
