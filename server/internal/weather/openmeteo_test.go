package weather

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestWMOLookup(t *testing.T) {
	icon, desc := describeWMO(3)
	if icon != "cloud" || desc != "Bewoelkt" {
		t.Errorf("code 3: got (%q, %q)", icon, desc)
	}
	icon, desc = describeWMO(95)
	if icon != "cloud-lightning" || desc != "Gewitter" {
		t.Errorf("code 95: got (%q, %q)", icon, desc)
	}
	icon, desc = describeWMO(9999)
	if icon != "cloud" || desc != "Wetter unbekannt" {
		t.Errorf("unknown code fallback: got (%q, %q)", icon, desc)
	}
}

func TestGet_CachesWithinFreshTTL(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"current":{"temperature_2m":11.4,"weather_code":3}}`))
	}))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL))
	for i := 0; i < 5; i++ {
		snap, err := c.Get(context.Background(), 51.6144, 7.1959)
		if err != nil {
			t.Fatalf("Get #%d: %v", i, err)
		}
		if snap.WeatherCode != 3 || snap.Icon != "cloud" {
			t.Fatalf("snap mismatch: %+v", snap)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected 1 backend call, got %d", got)
	}
}

func TestGet_StaleServingOnBackendError(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			_, _ = w.Write([]byte(`{"current":{"temperature_2m":15.2,"weather_code":61}}`))
			return
		}
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()

	now := time.Now()
	clock := func() time.Time { return now }
	c := New(WithBaseURL(srv.URL), WithClock(clock))

	first, err := c.Get(context.Background(), 51.0, 7.0)
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	if first.WeatherCode != 61 {
		t.Fatalf("unexpected first snap: %+v", first)
	}

	// Advance past freshTTL so the next Get must hit the backend.
	now = now.Add(freshTTL + time.Minute)
	stale, err := c.Get(context.Background(), 51.0, 7.0)
	if err != nil {
		t.Fatalf("stale Get: %v", err)
	}
	if stale.WeatherCode != 61 {
		t.Fatalf("expected stale snap to match: %+v", stale)
	}
}

func TestGet_FailsWhenNoCacheAndBackendDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL))
	_, err := c.Get(context.Background(), 51.0, 7.0)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
}

func TestGet_DropsStaleBeyond24h(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			_, _ = w.Write([]byte(`{"current":{"temperature_2m":10,"weather_code":3}}`))
			return
		}
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()

	now := time.Now()
	c := New(WithBaseURL(srv.URL), WithClock(func() time.Time { return now }))

	if _, err := c.Get(context.Background(), 51.0, 7.0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	now = now.Add(staleTTL + time.Hour)
	if _, err := c.Get(context.Background(), 51.0, 7.0); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable after stale window, got %v", err)
	}
}
