package weather

import (
	"sync"
	"time"
)

// freshTTL bounds how long a cached snapshot is treated as fresh.
// During that window Get returns the cached value without
// touching the network.
const freshTTL = 15 * time.Minute

// staleTTL is the upper bound for serving a stale snapshot when
// open-meteo is unreachable. Beyond this, Get returns the live
// error instead so the UI can hide the weather block.
const staleTTL = 24 * time.Hour

// cacheKey groups the (lat, lon) pair into a single map key.
// Floats are quantised to 4 decimals (~11 m at the equator) so
// minor drift on the admin side does not invalidate the cache.
type cacheKey struct {
	lat int
	lon int
}

func newCacheKey(lat, lon float64) cacheKey {
	return cacheKey{
		lat: int(lat*10000 + 0.5),
		lon: int(lon*10000 + 0.5),
	}
}

type cacheEntry struct {
	snap  Snapshot
	saved time.Time
}

// cache is the per-Client memo store. Read-mostly so a single
// RWMutex is fine.
type cache struct {
	mu   sync.RWMutex
	rows map[cacheKey]cacheEntry
	now  func() time.Time
}

func newCache(now func() time.Time) *cache {
	if now == nil {
		now = time.Now
	}
	return &cache{
		rows: make(map[cacheKey]cacheEntry),
		now:  now,
	}
}

// fresh returns the cached snapshot if it is younger than
// freshTTL. The caller uses this to skip the network call
// entirely.
func (c *cache) fresh(key cacheKey) (Snapshot, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.rows[key]
	if !ok {
		return Snapshot{}, false
	}
	if c.now().Sub(entry.saved) > freshTTL {
		return Snapshot{}, false
	}
	return entry.snap, true
}

// stale returns the cached snapshot if it exists and is younger
// than staleTTL, regardless of freshness. The caller uses this
// as a fallback after a failed network call.
func (c *cache) stale(key cacheKey) (Snapshot, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.rows[key]
	if !ok {
		return Snapshot{}, false
	}
	if c.now().Sub(entry.saved) > staleTTL {
		return Snapshot{}, false
	}
	return entry.snap, true
}

// store overwrites the cache entry for key. Both fresh and stale
// reads go through this single slot per location.
func (c *cache) store(key cacheKey, snap Snapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rows[key] = cacheEntry{snap: snap, saved: c.now()}
}
