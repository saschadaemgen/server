// Package sensorhistory is the STORED path of Sensor History H1: readings
// aggregated into interval buckets as a mean and persisted to SQLite
// (migration 041), plus the range query H2 (charts) consumes. It is the
// second of the two data paths - the live output (current reading, delivered
// the instant it arrives to the editor block + cockpit, what the climate
// loop evaluates) is real-time and never touches this package.
//
// The layer is source-neutral: it stores (device, metric, value) tuples and
// knows nothing about UniFi Protect. The recorder taps the reading stream
// (protectmonitor today, a push websocket later) and never blocks it.
package sensorhistory

import (
	"context"
	"database/sql"
	"fmt"
)

// Sample is one stored interval bucket: the mean Value of N readings in
// [TS, TS+interval) for a (DeviceID, Metric). TS is the bucket start in
// unix milliseconds.
type Sample struct {
	DeviceID string  `json:"device"`
	Metric   string  `json:"metric"`
	TS       int64   `json:"ts"`
	Value    float64 `json:"value"`
	N        int     `json:"n"`
}

// Store is the SQL gateway for sensor_samples. It delegates to *sql.DB, so
// it is safe for concurrent use.
type Store struct {
	db *sql.DB
}

// New constructs a Store over the shared database.
func New(db *sql.DB) *Store { return &Store{db: db} }

// Insert writes interval buckets. A re-flush of an existing bucket REPLACEs
// it on the primary key (device, metric, ts), so a boundary race can never
// double-count. A single transaction keeps a flush batch atomic.
func (s *Store) Insert(ctx context.Context, samples ...Sample) error {
	if len(samples) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO sensor_samples (device_id, metric, ts, value, n) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, sm := range samples {
		if _, err := stmt.ExecContext(ctx, sm.DeviceID, sm.Metric, sm.TS, sm.Value, sm.N); err != nil {
			return fmt.Errorf("sensorhistory: insert sample: %w", err)
		}
	}
	return tx.Commit()
}

// Query returns a device's metric over [fromMs, toMs] inclusive, ordered by
// ts ascending. When the raw bucket count would exceed maxPoints (a wide
// range at fine resolution), it downsamples to at most ~maxPoints coarser
// buckets using a WEIGHTED mean, SUM(value*n)/SUM(n) - the n column - so a
// year of minute data returns a bounded, correctly-averaged series rather
// than half a million rows. maxPoints <= 0 disables downsampling (raw
// buckets). fromMs > toMs yields an empty result.
func (s *Store) Query(ctx context.Context, deviceID, metric string, fromMs, toMs int64, maxPoints int) ([]Sample, error) {
	if fromMs > toMs {
		return nil, nil
	}
	if maxPoints > 0 {
		var raw int
		var minTS, maxTS sql.NullInt64
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*), MIN(ts), MAX(ts) FROM sensor_samples WHERE device_id=? AND metric=? AND ts BETWEEN ? AND ?`,
			deviceID, metric, fromMs, toMs).Scan(&raw, &minTS, &maxTS); err != nil {
			return nil, fmt.Errorf("sensorhistory: count: %w", err)
		}
		if raw > maxPoints {
			// The bucket width follows the span of the DATA, not of the
			// requested window. An "all time" chart asks from=0, and deriving
			// the width from that would spread maxPoints buckets across five
			// decades of empty epoch and collapse a month of samples into a
			// handful of points. The rows are keyed by (device, metric, ts),
			// so raw > maxPoints implies at least maxPoints distinct
			// milliseconds and the width stays >= 1.
			bucketMs := (maxTS.Int64 - minTS.Int64) / int64(maxPoints)
			if bucketMs < 1 {
				bucketMs = 1
			}
			rows, err := s.db.QueryContext(ctx, `
				SELECT (ts/?)*? AS bts, SUM(value*n)/SUM(n) AS v, SUM(n) AS nn
				FROM sensor_samples
				WHERE device_id=? AND metric=? AND ts BETWEEN ? AND ?
				GROUP BY ts/? ORDER BY bts`,
				bucketMs, bucketMs, deviceID, metric, fromMs, toMs, bucketMs)
			if err != nil {
				return nil, fmt.Errorf("sensorhistory: downsample query: %w", err)
			}
			return scanSamples(rows, deviceID, metric)
		}
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT ts, value, n FROM sensor_samples WHERE device_id=? AND metric=? AND ts BETWEEN ? AND ? ORDER BY ts`,
		deviceID, metric, fromMs, toMs)
	if err != nil {
		return nil, fmt.Errorf("sensorhistory: query: %w", err)
	}
	return scanSamples(rows, deviceID, metric)
}

// scanSamples reads (ts, value, n) rows into Samples, stamping the fixed
// device/metric (the query already filtered on them).
func scanSamples(rows *sql.Rows, deviceID, metric string) ([]Sample, error) {
	defer func() { _ = rows.Close() }()
	var out []Sample
	for rows.Next() {
		sm := Sample{DeviceID: deviceID, Metric: metric}
		if err := rows.Scan(&sm.TS, &sm.Value, &sm.N); err != nil {
			return nil, fmt.Errorf("sensorhistory: scan: %w", err)
		}
		out = append(out, sm)
	}
	return out, rows.Err()
}

// Prune deletes samples with ts strictly older than cutoffMs for one device,
// or for ALL devices when deviceID is "". It returns the number of rows
// removed. This is the age-based auto-delete retention (§4); rolling older
// data up to coarser buckets before deletion is the flagged follow-up the n
// column already supports.
func (s *Store) Prune(ctx context.Context, deviceID string, cutoffMs int64) (int64, error) {
	var res sql.Result
	var err error
	if deviceID == "" {
		res, err = s.db.ExecContext(ctx, `DELETE FROM sensor_samples WHERE ts < ?`, cutoffMs)
	} else {
		res, err = s.db.ExecContext(ctx, `DELETE FROM sensor_samples WHERE device_id=? AND ts < ?`, deviceID, cutoffMs)
	}
	if err != nil {
		return 0, fmt.Errorf("sensorhistory: prune: %w", err)
	}
	return res.RowsAffected()
}

// MetricSpan is one metric a device actually has stored, with the extent of
// what survives retention: First/Last are bucket-start milliseconds and N is
// the stored bucket count (not the reading count - each bucket already
// averages N readings of its own).
type MetricSpan struct {
	Metric string `json:"metric"`
	First  int64  `json:"first"`
	Last   int64  `json:"last"`
	N      int64  `json:"n"`
}

// Metrics lists the metrics a device has stored samples for, each with its
// retained extent. It is the discovery half the charts (H2) need: the
// capability catalog says what a device *can* report, this says what was
// actually *recorded*, so a chart can tell "no data yet" from "never
// recorded" instead of drawing an empty axis. The (device_id, metric, ts)
// primary key serves the grouping directly.
func (s *Store) Metrics(ctx context.Context, deviceID string) ([]MetricSpan, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT metric, MIN(ts), MAX(ts), COUNT(*)
		FROM sensor_samples WHERE device_id=?
		GROUP BY metric ORDER BY metric`, deviceID)
	if err != nil {
		return nil, fmt.Errorf("sensorhistory: metrics: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []MetricSpan
	for rows.Next() {
		var m MetricSpan
		if err := rows.Scan(&m.Metric, &m.First, &m.Last, &m.N); err != nil {
			return nil, fmt.Errorf("sensorhistory: metrics scan: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Devices returns the distinct device ids that have stored samples, for the
// retention sweep (each device pruned by its own resolved retention).
func (s *Store) Devices(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT device_id FROM sensor_samples`)
	if err != nil {
		return nil, fmt.Errorf("sensorhistory: devices: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
