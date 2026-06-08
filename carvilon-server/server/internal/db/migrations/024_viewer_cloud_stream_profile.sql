-- Saison 19-47: per-viewer CLOUD stream profile (the second profile field).
--
-- A viewer now carries TWO profile choices: the existing stream_profile is
-- the LAN profile (local /offer + edge_whep + ESP MJPEG paths), and this
-- new cloud_stream_profile is the profile the edge's cloud WHIP-publish
-- uses for the remote/4G path. A viewer pulls two streams (local high
-- passthrough + cloud re-encode), so it needs two independent choices.
--
-- Replaces the removed CARVILON_CLOUD_STREAM_PROFILE env flag - a hidden
-- global switch that forced ONE cloud profile onto ALL viewers. The choice
-- is now per-viewer and admin-managed in the viewer-detail UI (two selects
-- fed from the live /api/profiles list).
--
-- Nullable TEXT. NULL (the ALTER default for existing + new rows) means
-- "no cloud override": the cloud publish falls back to the viewer's LAN
-- resolution (ResolveStreamProfile) - exactly today's behaviour, no break.
-- The admin sets a distinct cloud profile per viewer when wanted.

ALTER TABLE viewers ADD COLUMN cloud_stream_profile TEXT;

INSERT INTO schema_version (version, applied_at)
VALUES (24, strftime('%s','now')*1000);
