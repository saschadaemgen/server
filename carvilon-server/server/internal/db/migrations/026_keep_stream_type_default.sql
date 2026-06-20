-- Saison 20: the two keep-stream-in-background flags get a VIEWER-TYPE
-- dependent default. The Android app must default to "stay connected" (true)
-- while the ESP keeps "close the stream cleanly" (false).
--
-- Migration 025 added both columns with a hard DDL DEFAULT 0, so every
-- existing AND new row stores 0 - there is no way to tell "unset" from
-- "explicitly off", which a per-type default needs. Fix: re-create the two
-- columns the way every other settings column works - NULLABLE, NO DDL
-- default - so an unset value is SQL NULL and the resolver in viewermanager
-- applies the per-type default (ESP false, Android/web true). An explicit
-- stored 0/1 keeps winning.
--
-- SQLite cannot drop a column default in place, so RENAME the DEFAULT-0
-- columns aside, ADD fresh defaultless ones, copy the EXPLICIT true (1)
-- through, then DROP the old columns. Everything that was 0 (the 025 backfill,
-- incl. a possible Android test viewer that stored 0) maps to NULL = unset and
-- therefore to its type default - the connected Android side, as intended.
-- New rows land as NULL too: viewerstore.Insert omits both columns and there
-- is no DDL default anymore.
--
-- ALTER TABLE RENAME/ADD/DROP COLUMN is supported by modernc.org/sqlite
-- (SQLite >= 3.35); migration 008 already drops a viewers column the same way.
-- The two columns carry no index or trigger, so nothing else needs touching.

ALTER TABLE viewers RENAME COLUMN keep_stream_in_screensaver TO keep_stream_in_screensaver_old;
ALTER TABLE viewers RENAME COLUMN keep_stream_in_screen_off  TO keep_stream_in_screen_off_old;

ALTER TABLE viewers ADD COLUMN keep_stream_in_screensaver INTEGER;
ALTER TABLE viewers ADD COLUMN keep_stream_in_screen_off  INTEGER;

UPDATE viewers SET keep_stream_in_screensaver = 1 WHERE keep_stream_in_screensaver_old = 1;
UPDATE viewers SET keep_stream_in_screen_off  = 1 WHERE keep_stream_in_screen_off_old  = 1;

ALTER TABLE viewers DROP COLUMN keep_stream_in_screensaver_old;
ALTER TABLE viewers DROP COLUMN keep_stream_in_screen_off_old;

INSERT INTO schema_version (version, applied_at)
VALUES (26, strftime('%s','now')*1000);
