// Package droplog provides a rate-limited "X frames dropped" log helper.
//
// Live video prefers freshness over completeness — we drop frames silently
// when something is structurally wrong, but we still want one diagnostic
// line in the log so a real anomaly is visible. Per-packet logging would
// bury every other useful message; this helper emits at most one summary
// line per second carrying the count of drops in the window and the most
// recent error seen.
//
// Used at every drop-statt-buffer boundary: source → consumer, consumer →
// pion, etc. The same counter shape, never re-implemented.
package droplog

import (
	"log"
	"sync"
	"time"
)

// Counter rate-limits drop notifications to one per second.
//
// Zero value is usable; set Logger / Label before the first Record call,
// or rely on log.Default and the default "drop" label.
type Counter struct {
	Logger *log.Logger
	Label  string // prefix on the emitted log line, e.g. "unifi: depacketize"

	mu      sync.Mutex
	lastAt  time.Time
	pending int
}

// Record notes one dropped item. The provided err is included in the
// next emitted log line if one fires for this Record.
func (c *Counter) Record(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.pending++
	if time.Since(c.lastAt) < time.Second {
		return
	}

	logger := c.Logger
	if logger == nil {
		logger = log.Default()
	}
	label := c.Label
	if label == "" {
		label = "drop"
	}
	logger.Printf("%s: dropped %d; last err: %v", label, c.pending, err)
	c.lastAt = time.Now()
	c.pending = 0
}
