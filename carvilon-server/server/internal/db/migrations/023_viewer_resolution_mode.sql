-- Saison 19-42: per-viewer source-resolution choice (the third switch).
--
-- 'high' (1200x1600) / 'medium' (960x1280) / 'low' (360x480). The three
-- RTSPS source steps already exist on the stream side; THIS is only the
-- CHOICE, not the pull. carvilon stores + serves the chosen step; the
-- stream pulls it and the app uses it at stream-start.
--
-- Default 'medium': the resource-friendly choice for the cloud/Android path
-- (the phone renders ~960 anyway; medium GOP 75 vs high 105 -> fewer
-- resources, shorter freezes). 'high' is selected in LAN / via the switch.
-- A later weg-abhaengige refinement (LAN=high) is App/Stream, NOT here.
--
-- NOT NULL DEFAULT 'medium' -> existing + new rows are 'medium', no backfill.

ALTER TABLE viewers ADD COLUMN resolution_mode TEXT NOT NULL DEFAULT 'medium';

INSERT INTO schema_version (version, applied_at)
VALUES (23, strftime('%s','now')*1000);
