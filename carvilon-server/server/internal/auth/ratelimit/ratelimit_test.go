package ratelimit

import (
	"sync/atomic"
	"testing"
	"time"
)

func mockClock() (*Limiter, *atomic.Int64) {
	var nowMs atomic.Int64
	nowMs.Store(time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC).UnixMilli())
	return NewWithClock(func() time.Time {
		return time.UnixMilli(nowMs.Load())
	}), &nowMs
}

func advance(now *atomic.Int64, d time.Duration) {
	now.Add(d.Milliseconds())
}

func TestAllow_FirstHitOK(t *testing.T) {
	l, _ := mockClock()
	if l.Allow("1.1.1.1", "alice") != Allow {
		t.Error("first attempt should be Allow")
	}
}

func TestAllow_BlocksAfterIPThreshold(t *testing.T) {
	l, _ := mockClock()
	for i := 0; i < IPMaxAttempts; i++ {
		l.RegisterFailure("1.1.1.1", "")
	}
	if got := l.Allow("1.1.1.1", ""); got != BlockedByIP {
		t.Errorf("after %d failures got %v, want BlockedByIP", IPMaxAttempts, got)
	}
}

func TestAllow_BlocksAfterUserThreshold(t *testing.T) {
	l, _ := mockClock()
	for i := 0; i < UserLockoutThreshold; i++ {
		l.RegisterFailure("", "alice")
	}
	if got := l.Allow("", "alice"); got != BlockedByUser {
		t.Errorf("after %d failures got %v, want BlockedByUser (lockout)", UserLockoutThreshold, got)
	}
}

func TestAllow_LockoutExpires(t *testing.T) {
	l, now := mockClock()
	for i := 0; i < UserLockoutThreshold; i++ {
		l.RegisterFailure("", "alice")
	}
	if got := l.Allow("", "alice"); got != BlockedByUser {
		t.Fatalf("expected lockout, got %v", got)
	}
	advance(now, UserLockoutDuration+time.Second)
	if got := l.Allow("", "alice"); got != Allow {
		t.Errorf("after lockout expiry got %v, want Allow", got)
	}
}

func TestAllow_IPWindowExpires(t *testing.T) {
	l, now := mockClock()
	for i := 0; i < IPMaxAttempts; i++ {
		l.RegisterFailure("1.1.1.1", "")
	}
	if got := l.Allow("1.1.1.1", ""); got != BlockedByIP {
		t.Fatalf("expected IP block, got %v", got)
	}
	advance(now, IPWindow+time.Second)
	if got := l.Allow("1.1.1.1", ""); got != Allow {
		t.Errorf("after window expiry got %v, want Allow", got)
	}
}

func TestRegisterSuccess_ClearsUserCounter(t *testing.T) {
	l, _ := mockClock()
	for i := 0; i < UserMaxAttempts-1; i++ {
		l.RegisterFailure("", "alice")
	}
	l.RegisterSuccess("alice")
	if got := l.Allow("", "alice"); got != Allow {
		t.Errorf("after success got %v, want Allow", got)
	}
}

func TestClearUser_LiftsLockout(t *testing.T) {
	l, _ := mockClock()
	for i := 0; i < UserLockoutThreshold; i++ {
		l.RegisterFailure("", "alice")
	}
	l.ClearUser("alice")
	if got := l.Allow("", "alice"); got != Allow {
		t.Errorf("after admin unlock got %v, want Allow", got)
	}
}

func TestClearIP_LiftsBlock(t *testing.T) {
	l, _ := mockClock()
	for i := 0; i < IPMaxAttempts; i++ {
		l.RegisterFailure("1.1.1.1", "")
	}
	l.ClearIP("1.1.1.1")
	if got := l.Allow("1.1.1.1", ""); got != Allow {
		t.Errorf("after admin unlock got %v, want Allow", got)
	}
}

func TestLockedUsers_ListsActiveLockouts(t *testing.T) {
	l, _ := mockClock()
	for i := 0; i < UserLockoutThreshold; i++ {
		l.RegisterFailure("", "alice")
	}
	locks := l.LockedUsers()
	if len(locks) != 1 {
		t.Fatalf("LockedUsers() = %d, want 1", len(locks))
	}
	if locks[0].Value != "alice" {
		t.Errorf("locked user = %q", locks[0].Value)
	}
	if locks[0].Kind != "user" {
		t.Errorf("kind = %q", locks[0].Kind)
	}
}

func TestHotIPs_ListsActiveOffenders(t *testing.T) {
	l, _ := mockClock()
	for i := 0; i < IPMaxAttempts; i++ {
		l.RegisterFailure("1.1.1.1", "")
	}
	for i := 0; i < 2; i++ {
		l.RegisterFailure("2.2.2.2", "")
	}
	hot := l.HotIPs(IPMaxAttempts)
	if len(hot) != 1 {
		t.Fatalf("HotIPs() = %d, want 1", len(hot))
	}
	if hot[0].Value != "1.1.1.1" {
		t.Errorf("hot ip = %q", hot[0].Value)
	}
}
