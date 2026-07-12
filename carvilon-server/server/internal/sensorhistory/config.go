package sensorhistory

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"
)

// Global default recording knobs - the fallback when a sensor has no
// override. The interval is the averaging / rollup granularity; retention is
// the auto-delete age. Both are Sascha-confirmable and per-sensor overridable
// in the sensor's settings.
const (
	DefaultInterval  = time.Minute         // §3 suggested default
	DefaultRetention = 30 * 24 * time.Hour // §4 sensible global default
	MinInterval      = time.Second         // §3 lower bound
	MaxFineInterval  = 15 * time.Minute    // §3 upper bound of the fine range
	HourlyInterval   = time.Hour           // the one preset above the fine range
)

// Config is the resolved recording knobs for one sensor.
type Config struct {
	Interval  time.Duration
	Retention time.Duration
}

// override is a raw per-sensor row; an invalid (NULL) field inherits the
// global default.
type override struct {
	intervalSec  sql.NullInt64
	retentionSec sql.NullInt64
}

// ConfigStore holds the per-sensor recording overrides (sensor_recording)
// behind an in-memory cache, so the recorder resolves a device's interval on
// every reading without a SQL round-trip. Safe for concurrent use.
type ConfigStore struct {
	db  *sql.DB
	now func() time.Time

	defInterval  time.Duration
	defRetention time.Duration

	mu    sync.RWMutex
	cache map[string]override
}

// NewConfigStore constructs a ConfigStore with the global defaults. Call
// Load once to warm the cache from the table.
func NewConfigStore(db *sql.DB) *ConfigStore {
	return &ConfigStore{
		db:           db,
		now:          time.Now,
		defInterval:  DefaultInterval,
		defRetention: DefaultRetention,
		cache:        map[string]override{},
	}
}

// Load fills the cache from sensor_recording. Call once at startup.
func (c *ConfigStore) Load(ctx context.Context) error {
	rows, err := c.db.QueryContext(ctx, `SELECT device_id, interval_sec, retention_sec FROM sensor_recording`)
	if err != nil {
		return fmt.Errorf("sensorhistory: load config: %w", err)
	}
	defer func() { _ = rows.Close() }()
	m := map[string]override{}
	for rows.Next() {
		var id string
		var o override
		if err := rows.Scan(&id, &o.intervalSec, &o.retentionSec); err != nil {
			return err
		}
		m[id] = o
	}
	if err := rows.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.cache = m
	c.mu.Unlock()
	return nil
}

// Get resolves a device's effective config (override where set, else the
// global default), from the cache only - no SQL, safe on the hot path.
func (c *ConfigStore) Get(deviceID string) Config {
	c.mu.RLock()
	o, ok := c.cache[deviceID]
	c.mu.RUnlock()
	cfg := Config{Interval: c.defInterval, Retention: c.defRetention}
	if ok {
		if o.intervalSec.Valid && o.intervalSec.Int64 > 0 {
			cfg.Interval = time.Duration(o.intervalSec.Int64) * time.Second
		}
		if o.retentionSec.Valid && o.retentionSec.Int64 > 0 {
			cfg.Retention = time.Duration(o.retentionSec.Int64) * time.Second
		}
	}
	return cfg
}

// Set writes a device's overrides and updates the cache. A value <= 0 clears
// that field back to the global default; the interval is clamped to the
// allowed range (1 s..15 min, or exactly hourly).
func (c *ConfigStore) Set(ctx context.Context, deviceID string, intervalSec, retentionSec int64) error {
	intervalSec = clampInterval(intervalSec)
	if retentionSec < 0 {
		retentionSec = 0
	}
	o := override{}
	if intervalSec > 0 {
		o.intervalSec = sql.NullInt64{Int64: intervalSec, Valid: true}
	}
	if retentionSec > 0 {
		o.retentionSec = sql.NullInt64{Int64: retentionSec, Valid: true}
	}
	_, err := c.db.ExecContext(ctx, `
		INSERT INTO sensor_recording (device_id, interval_sec, retention_sec, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(device_id) DO UPDATE SET
			interval_sec=excluded.interval_sec,
			retention_sec=excluded.retention_sec,
			updated_at=excluded.updated_at`,
		deviceID, o.intervalSec, o.retentionSec, c.now().UnixMilli())
	if err != nil {
		return fmt.Errorf("sensorhistory: set config: %w", err)
	}
	c.mu.Lock()
	c.cache[deviceID] = o
	c.mu.Unlock()
	return nil
}

// Raw returns a device's stored override seconds (0 = inherit the default),
// for the settings UI to prefill the fields.
func (c *ConfigStore) Raw(deviceID string) (intervalSec, retentionSec int64) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	o := c.cache[deviceID]
	if o.intervalSec.Valid {
		intervalSec = o.intervalSec.Int64
	}
	if o.retentionSec.Valid {
		retentionSec = o.retentionSec.Int64
	}
	return
}

// Defaults returns the global default knobs (for the settings UI to show what
// "inherit" resolves to).
func (c *ConfigStore) Defaults() Config {
	return Config{Interval: c.defInterval, Retention: c.defRetention}
}

// clampInterval snaps a requested interval (seconds) to the allowed set:
// 0 (inherit default), the fine range 1 s..15 min, or exactly hourly.
func clampInterval(sec int64) int64 {
	switch {
	case sec <= 0:
		return 0
	case sec == int64(HourlyInterval/time.Second):
		return sec
	case sec > int64(MaxFineInterval/time.Second):
		return int64(MaxFineInterval / time.Second)
	default:
		return sec
	}
}
