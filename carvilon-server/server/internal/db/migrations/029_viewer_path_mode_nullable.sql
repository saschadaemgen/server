-- Saison 20: make viewers.path_mode NULLABLE so it can inherit (template /
-- per-type default), exactly like keep_stream in migration 026.
--
-- Migration 021 added path_mode as NOT NULL DEFAULT 'auto', so every existing
-- and new row stores 'auto' and "unset" is indistinguishable from "explicitly
-- auto". Now that path_mode is a catalog function carrying the exposure
-- controls, it must be able to inherit a value the same way keep_stream does.
-- Fix in the exact 026 pattern: rename the DEFAULT-'auto' column aside, add a
-- fresh defaultless (NULLABLE) one, copy only the EXPLICIT non-default values
-- (local / cloud) through, then drop the old column. Rows that were 'auto' map
-- to NULL = unset = inherit; the resolver already COALESCEs NULL -> 'auto', so
-- behaviour is unchanged. New rows land as NULL (no DDL default anymore).
--
-- ALTER TABLE RENAME/ADD/DROP COLUMN is supported by modernc.org/sqlite
-- (>= 3.35); migrations 008 and 026 do the same. path_mode carries no index or
-- trigger, so nothing else needs touching.

ALTER TABLE viewers RENAME COLUMN path_mode TO path_mode_old;

ALTER TABLE viewers ADD COLUMN path_mode TEXT;

UPDATE viewers SET path_mode = path_mode_old
 WHERE path_mode_old IS NOT NULL AND path_mode_old != 'auto';

ALTER TABLE viewers DROP COLUMN path_mode_old;

INSERT INTO schema_version (version, applied_at)
VALUES (29, strftime('%s','now')*1000);
