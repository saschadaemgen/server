// Package ratelimit ist ein in-memory Anti-Brute-Force-Limiter
// fuer den Web-Viewer- und Admin-Login. Drei unabhaengige Zaehler:
//
//	IP        : 5 Fehlversuche pro 15 Minuten
//	Username  : 10 Fehlversuche pro 60 Minuten
//	Lockout   : nach 10 Fehlversuchen pro Username -> 5 Minuten Sperre
//
// Erfolgreicher Login (RegisterSuccess) loescht den Username-
// Zaehler und nimmt eine bestehende Sperre weg. IP-Zaehler bleibt
// stehen damit IP-weite Brute-Force-Versuche nicht via einzelnem
// erfolgreichem Account zurueckgesetzt werden koennen.
//
// Storage ist Process-Memory mit sync.RWMutex. Reichlich genug
// fuer eine Hausverwaltungs-Plattform; bei mehreren Servern oder
// bei Server-Restart gehen Zaehler verloren (akzeptierte Trade-Off,
// wir wollen kein Redis im RPi-Stack).
package ratelimit

import (
	"sort"
	"sync"
	"time"
)

// Default-Schwellen.
const (
	IPMaxAttempts        = 5
	IPWindow             = 15 * time.Minute
	UserMaxAttempts      = 10
	UserWindow           = 60 * time.Minute
	UserLockoutThreshold = 10
	UserLockoutDuration  = 5 * time.Minute
)

// Decision beschreibt das Ergebnis eines Allow-Calls.
type Decision int

const (
	// Allow heisst: Login darf versucht werden.
	Allow Decision = iota
	// BlockedByIP heisst: zu viele Versuche aus dieser IP.
	BlockedByIP
	// BlockedByUser heisst: zu viele Versuche fuer diesen Username
	// (account-lockout aktiv).
	BlockedByUser
)

// Lock ist ein Snapshot einer aktiven Sperre, fuer die Settings-
// Liste im Admin-UI.
type Lock struct {
	Kind        string // "ip" oder "user"
	Value       string
	Attempts    int
	LockedUntil time.Time
}

// Limiter ist die Live-Datenstruktur.
type Limiter struct {
	mu              sync.RWMutex
	ipHits          map[string][]time.Time
	userHits        map[string][]time.Time
	userLockedUntil map[string]time.Time
	now             func() time.Time
}

// New baut einen frischen Limiter mit time.Now als Clock-Quelle.
func New() *Limiter {
	return NewWithClock(time.Now)
}

// NewWithClock injiziert eine Test-Clock.
func NewWithClock(now func() time.Time) *Limiter {
	return &Limiter{
		ipHits:          make(map[string][]time.Time),
		userHits:        make(map[string][]time.Time),
		userLockedUntil: make(map[string]time.Time),
		now:             now,
	}
}

// Allow prueft ob ein Login fuer (ip, username) gerade zulaessig
// ist. Decision == Allow -> caller darf den Hash-Compare machen.
// Bei Block braucht caller nichts mehr tun ausser dem User Bescheid
// zu sagen ("Zu viele Versuche").
func (l *Limiter) Allow(ip, username string) Decision {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()

	// Lockout vorrangig: wenn Username gerade gesperrt ist, blocken.
	if until, ok := l.userLockedUntil[username]; ok {
		if now.Before(until) {
			return BlockedByUser
		}
		// Sperre abgelaufen, Lockout-Eintrag entsorgen und Zaehler
		// auch zuruecksetzen damit der User wieder von 0 anfangen
		// kann.
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

// RegisterFailure zaehlt einen Fehlversuch und triggert ggf. einen
// Account-Lockout. Caller ruft das nach einer fehlgeschlagenen
// Hash-Verifikation auf.
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

// RegisterSuccess loescht die Sperren und Zaehler fuer den
// Username, weil ein erfolgreicher Login zeigt dass der User legit
// ist. IP-Zaehler bleiben stehen (siehe Paket-Doku).
func (l *Limiter) RegisterSuccess(username string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.userHits, username)
	delete(l.userLockedUntil, username)
}

// ClearUser hebt die Sperre fuer einen Username manuell auf
// (Admin-Aktion). Auch der Hit-Counter wird geleert.
func (l *Limiter) ClearUser(username string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.userHits, username)
	delete(l.userLockedUntil, username)
}

// ClearIP hebt die IP-Sperre manuell auf (Admin-Aktion).
func (l *Limiter) ClearIP(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.ipHits, ip)
}

// LockedUsers liefert eine Liste der aktuell gesperrten Usernamen
// (sortiert nach Wartezeit, kuerzeste zuerst). Fuer das Admin-UI.
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

// HotIPs liefert IPs mit mindestens floor Fehlversuchen im
// IP-Fenster. Fuer das Admin-UI ("aktive Brute-Force-Quellen").
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

// pruneOlder filtert alle Zeitpunkte raus, die vor cutoff liegen.
// Schreibt direkt im uebergebenen Slice (in-place).
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
