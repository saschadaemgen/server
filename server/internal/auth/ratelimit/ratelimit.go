// Package ratelimit is an in-memory anti-brute-force limiter for
// the web-viewer and admin login. Three independent counters:
//
//	IP       : 5 failed attempts per 15 minutes
//	Username : 10 failed attempts per 60 minutes
//	Lockout  : after 10 failures per username -> 5-minute lockout
//
// A successful login (RegisterSuccess) clears the username
// counter and lifts any active lockout for that user. The IP
// counter is intentionally NOT cleared, so a wide brute-force
// run against a single host cannot be reset via one lucky
// account.
//
// Storage is process memory guarded by a sync.RWMutex. Plenty
// for a single-house deployment; with multiple servers or after
// a server restart the counters are lost (accepted trade-off:
// we do not want Redis in the RPi stack).
package ratelimit

import (
	"sort"
	"sync"
	"time"
)

// Default thresholds.
const (
	IPMaxAttempts        = 5
	IPWindow             = 15 * time.Minute
	UserMaxAttempts      = 10
	UserWindow           = 60 * time.Minute
	UserLockoutThreshold = 10
	UserLockoutDuration  = 5 * time.Minute
)

// Decision is the outcome of an Allow call.
type Decision int

const (
	// Allow means: the login may be attempted.
	Allow Decision = iota
	// BlockedByIP means: too many attempts from this IP.
	BlockedByIP
	// BlockedByUser means: too many attempts for this username
	// (account lockout active).
	BlockedByUser
)

// Lock is a snapshot of an active lockout, used by the
// settings list in the admin UI.
type Lock struct {
	Kind        string // "ip" or "user"
	Value       string
	Attempts    int
	LockedUntil time.Time
}

// Limiter is the live data structure.
type Limiter struct {
	mu              sync.RWMutex
	ipHits          map[string][]time.Time
	userHits        map[string][]time.Time
	userLockedUntil map[string]time.Time
	now             func() time.Time
}

// New builds a fresh Limiter with time.Now as the clock source.
func New() *Limiter {
	return NewWithClock(time.Now)
}

// NewWithClock injects a test clock.
func NewWithClock(now func() time.Time) *Limiter {
	return &Limiter{
		ipHits:          make(map[string][]time.Time),
		userHits:        make(map[string][]time.Time),
		userLockedUntil: make(map[string]time.Time),
		now:             now,
	}
}

// Allow checks whether a login for (ip, username) is currently
// permitted. Decision == Allow -> caller may run the hash
// compare. On block the caller only needs to tell the user
// ("Zu viele Versuche").
func (l *Limiter) Allow(ip, username string) Decision {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()

	// Lockout takes precedence: if the username is currently
	// locked, block.
	if until, ok := l.userLockedUntil[username]; ok {
		if now.Before(until) {
			return BlockedByUser
		}
		// Lockout expired; drop the entry and also reset the
		// counter so the user can start fresh.
		delete(l.userLockedUntil, username)
		delete(l.userHits, username)
	}

	if ip != "" {
		l.ipHits[ip] = pruneOlder(l.ipHits[ip], now.Add(-IPWindow))
		if len(l.ipHits[ip]) >= IPMaxAttempts {
			return BlockedByIP
		}
	}
	if username != "" {
		l.userHits[username] = pruneOlder(l.userHits[username], now.Add(-UserWindow))
		if len(l.userHits[username]) >= UserMaxAttempts {
			return BlockedByUser
		}
	}
	return Allow
}

// RegisterFailure counts a failed attempt and, if the threshold
// is hit, triggers an account lockout. Callers invoke this after
// a failed hash verification.
func (l *Limiter) RegisterFailure(ip, username string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if ip != "" {
		l.ipHits[ip] = append(pruneOlder(l.ipHits[ip], now.Add(-IPWindow)), now)
	}
	if username != "" {
		l.userHits[username] = append(pruneOlder(l.userHits[username], now.Add(-UserWindow)), now)
		if len(l.userHits[username]) >= UserLockoutThreshold {
			l.userLockedUntil[username] = now.Add(UserLockoutDuration)
		}
	}
}

// RegisterSuccess clears the lockout and counters for the
// username, because a successful login proves the user is
// legitimate. IP counters are kept (see package doc).
func (l *Limiter) RegisterSuccess(username string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.userHits, username)
	delete(l.userLockedUntil, username)
}

// ClearUser manually lifts the lockout for a username
// (admin action). The hit counter is cleared as well.
func (l *Limiter) ClearUser(username string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.userHits, username)
	delete(l.userLockedUntil, username)
}

// ClearIP manually lifts the IP lockout (admin action).
func (l *Limiter) ClearIP(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.ipHits, ip)
}

// LockedUsers returns the currently locked-out usernames sorted
// by remaining wait time (shortest first). For the admin UI.
func (l *Limiter) LockedUsers() []Lock {
	l.mu.RLock()
	defer l.mu.RUnlock()
	now := l.now()
	out := make([]Lock, 0, len(l.userLockedUntil))
	for u, until := range l.userLockedUntil {
		if !now.Before(until) {
			continue
		}
		out = append(out, Lock{
			Kind:        "user",
			Value:       u,
			Attempts:    len(l.userHits[u]),
			LockedUntil: until,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LockedUntil.Before(out[j].LockedUntil)
	})
	return out
}

// HotIPs returns IPs with at least `floor` failed attempts in
// the current IP window. For the admin UI
// ("aktive Brute-Force-Quellen").
func (l *Limiter) HotIPs(floor int) []Lock {
	l.mu.RLock()
	defer l.mu.RUnlock()
	now := l.now()
	out := make([]Lock, 0)
	for ip, hits := range l.ipHits {
		alive := pruneOlder(hits, now.Add(-IPWindow))
		if len(alive) < floor {
			continue
		}
		out = append(out, Lock{
			Kind:     "ip",
			Value:    ip,
			Attempts: len(alive),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Attempts > out[j].Attempts
	})
	return out
}

// pruneOlder filters out all timestamps before cutoff. Writes
// into the passed-in slice in place.
func pruneOlder(ts []time.Time, cutoff time.Time) []time.Time {
	if len(ts) == 0 {
		return ts
	}
	out := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			out = append(out, t)
		}
	}
	return out
}
