-- Sensor history H1 (recording + storage + retention foundation).
--
-- TWO DATA PATHS, and only the STORED one lives here: the live output
-- (the current reading, delivered the instant it arrives to the editor
-- block + cockpit, what the climate loop evaluates) is real-time and is
-- NEVER stored. This table is the SECOND path: readings aggregated into
-- interval buckets as a MEAN, for charts (H2) and analysis. Both paths
-- feed from the same incoming reading stream, but recording never delays
-- the live output.
--
-- sensor_samples - one row per (device, metric, interval bucket):
--   device_id - the sensor's stable id (protectapi.Sensor.ID today; the
--               recorder is source-neutral, so any readout device fits).
--   metric    - the readout token ("temperature","humidity",...); a bool
--               metric (motion/leak) stores its duty fraction 0..1.
--   ts        - the bucket START, unix milliseconds. floor(reading_ms /
--               interval) * interval, so a bucket is [ts, ts+interval).
--   value     - the MEAN of every reading received during the bucket. One
--               reading -> that reading; NO reading -> no row (we do not
--               invent data; a gap is an honest gap the chart interpolates).
--   n         - how many readings the mean averaged. Kept so a wide-range
--               query can downsample by a WEIGHTED mean (SUM(value*n)/SUM(n))
--               and so older data can be rolled up to coarser buckets
--               before deletion without skewing the average - the schema is
--               downsampling/rollup-ready even though H1 prunes by plain age.
--
-- The PRIMARY KEY (device_id, metric, ts) is itself the range index SQLite
-- uses for the H2 query "device + metric over a time range", so no extra
-- index is needed. Survives restart (ordinary persisted table).
CREATE TABLE sensor_samples (
    device_id TEXT    NOT NULL,
    metric    TEXT    NOT NULL,
    ts        INTEGER NOT NULL,
    value     REAL    NOT NULL,
    n         INTEGER NOT NULL,
    PRIMARY KEY (device_id, metric, ts)
);

-- sensor_recording - the per-sensor recording knobs (§3/§4). A row exists
-- only when the operator has overridden a default; an absent row (or a NULL
-- column) means "use the global default" the recorder carries in code
-- (interval 60 s, retention 30 d). interval_sec is the averaging/rollup
-- granularity (1 s..15 min, plus hourly=3600); retention_sec is the
-- auto-delete age. Per sensor, edited in the sensor's settings.
CREATE TABLE sensor_recording (
    device_id     TEXT PRIMARY KEY,
    interval_sec  INTEGER,
    retention_sec INTEGER,
    updated_at    INTEGER NOT NULL
);

INSERT INTO schema_version (version, applied_at)
VALUES (41, strftime('%s','now')*1000);
