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

// TestResolveWeather_GermanDefault covers the original
// describeWMO call site (now resolveWeather with empty lang) and
// the umlaut update: code 3 used to render "Bewoelkt", which was
// later switched to "Bewölkt" with a real U+00FC.
func TestResolveWeather_GermanDefault(t *testing.T) {
	icon, desc := resolveWeather(3, "")
	if icon != "cloud" || desc != "Bewölkt" {
		t.Errorf("code 3 de: got (%q, %q)", icon, desc)
	}
	icon, desc = resolveWeather(95, "de")
	if icon != "cloud-lightning" || desc != "Gewitter" {
		t.Errorf("code 95 de: got (%q, %q)", icon, desc)
	}
	icon, desc = resolveWeather(9999, "de")
	if icon != "cloud" || desc != "Unbekannt" {
		t.Errorf("unknown code de fallback: got (%q, %q)", icon, desc)
	}
}

// TestResolveWeather_English covers the EN path: same icon,
// English label per the curated table; unknown codes fall back to
// "Unknown".
func TestResolveWeather_English(t *testing.T) {
	icon, desc := resolveWeather(3, "en")
	if icon != "cloud" || desc != "Overcast" {
		t.Errorf("code 3 en: got (%q, %q)", icon, desc)
	}
	icon, desc = resolveWeather(95, "en")
	if icon != "cloud-lightning" || desc != "Thunderstorm" {
		t.Errorf("code 95 en: got (%q, %q)", icon, desc)
	}
	icon, desc = resolveWeather(9999, "en")
	if icon != "cloud" || desc != "Unknown" {
		t.Errorf("unknown code en fallback: got (%q, %q)", icon, desc)
	}
}

// TestResolveWeather_UmlautBytes pins the actual UTF-8 byte
// sequence in known labels so a future refactor (or a stray IDE
// auto-conversion to ASCII) tripping the encoding gets caught at
// test-time instead of in a tenant's screensaver. Code 3 carries
// "Bewölkt" with U+00F6 LATIN SMALL LETTER O WITH DIAERESIS
// (0xC3 0xB6 in UTF-8); code 1 carries "Überwiegend klar" with
// U+00DC LATIN CAPITAL LETTER U WITH DIAERESIS (0xC3 0x9C in
// UTF-8). Both labels are explicit umlaut fixes - the earlier
// tables shipped ASCII placeholders ("Bewoelkt", "Heiter") that
// the ESP could not render with umlauts at all.
func TestResolveWeather_UmlautBytes(t *testing.T) {
	cases := []struct {
		code int
		want []byte
	}{
		// "Bewölkt" = B e w 0xC3 0xB6 l k t
		{code: 3, want: []byte{'B', 'e', 'w', 0xC3, 0xB6, 'l', 'k', 't'}},
		// "Überwiegend klar" starts with 0xC3 0x9C (U+00DC).
		{code: 1, want: []byte{0xC3, 0x9C, 'b', 'e', 'r', 'w', 'i', 'e', 'g', 'e', 'n', 'd', ' ', 'k', 'l', 'a', 'r'}},
	}
	for _, tc := range cases {
		_, desc := resolveWeather(tc.code, "de")
		got := []byte(desc)
		if len(got) != len(tc.want) {
			t.Errorf("code %d byte len = %d (%q), want %d", tc.code, len(got), desc, len(tc.want))
			continue
		}
		for i, b := range got {
			if b != tc.want[i] {
				t.Errorf("code %d byte %d = 0x%02X, want 0x%02X (desc=%q)",
					tc.code, i, b, tc.want[i], desc)
			}
		}
	}
}

// TestResolveWeather_UnknownLanguageFallsBackToGerman verifies
// the conservative default. An ESP that sends lang="fr" or any
// other unrecognised value still gets a renderable string instead
// of empty text.
func TestResolveWeather_UnknownLanguageFallsBackToGerman(t *testing.T) {
	icon, desc := resolveWeather(3, "fr")
	if icon != "cloud" || desc != "Bewölkt" {
		t.Errorf("unknown lang fallback: got (%q, %q)", icon, desc)
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
		snap, err := c.Get(context.Background(), 51.6144, 7.1959, "de")
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

	first, err := c.Get(context.Background(), 51.0, 7.0, "de")
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	if first.WeatherCode != 61 {
		t.Fatalf("unexpected first snap: %+v", first)
	}

	// Advance past freshTTL so the next Get must hit the backend.
	now = now.Add(freshTTL + time.Minute)
	stale, err := c.Get(context.Background(), 51.0, 7.0, "de")
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
	_, err := c.Get(context.Background(), 51.0, 7.0, "de")
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

	if _, err := c.Get(context.Background(), 51.0, 7.0, "de"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	now = now.Add(staleTTL + time.Hour)
	if _, err := c.Get(context.Background(), 51.0, 7.0, "de"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable after stale window, got %v", err)
	}
}
