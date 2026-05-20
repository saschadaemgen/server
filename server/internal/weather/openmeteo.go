package weather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// defaultBaseURL is the public open-meteo endpoint. No API key,
// no rate-limit, no signup; the home-server can hit it directly.
const defaultBaseURL = "https://api.open-meteo.com/v1/forecast"

// ErrUnavailable is returned when both the live call and the
// stale cache fail to produce a snapshot. Handlers map this to
// "weather block is hidden" rather than 5xx.
var ErrUnavailable = errors.New("weather: no fresh or stale snapshot available")

// Snapshot is what the screensaver renders and the /esp/config
// JSON ships. All fields are populated from one open-meteo call.
//
// Saison 14-FIX07: Description + Icon are filled at Get-time from
// the per-tenant language; the cache only stores the language-
// neutral fields below (TempC, WeatherCode, FetchedAt).
type Snapshot struct {
	TempC       float64   `json:"temp_c"`
	WeatherCode int       `json:"weather_code"`
	Description string    `json:"description"`
	Icon        string    `json:"icon"`
	FetchedAt   time.Time `json:"fetched_at"`
}

// rawSnapshot is the language-neutral payload that lives in the
// cache. Get adds the localized Description + Icon on top of it
// before returning to the caller.
type rawSnapshot struct {
	TempC       float64
	WeatherCode int
	FetchedAt   time.Time
}

// localize converts the language-neutral rawSnapshot into a
// Snapshot the handler can ship. lang is "de" / "en"; empty falls
// back to German via resolveWeather.
func (r rawSnapshot) localize(lang string) Snapshot {
	icon, desc := resolveWeather(r.WeatherCode, lang)
	return Snapshot{
		TempC:       r.TempC,
		WeatherCode: r.WeatherCode,
		Description: desc,
		Icon:        icon,
		FetchedAt:   r.FetchedAt,
	}
}

// Client is the open-meteo-facing facade. Construct with New;
// share between handlers (the cache is shared internally).
type Client struct {
	baseURL string
	cache   *cache
	httpC   *http.Client
	now     func() time.Time
}

// Option mutates a Client during construction. Tests use these
// to inject a fake HTTP client + clock.
type Option func(*Client)

// WithHTTPClient swaps the underlying http.Client; defaults to
// a 5-second-timeout DefaultClient-clone otherwise.
func WithHTTPClient(c *http.Client) Option {
	return func(w *Client) { w.httpC = c }
}

// WithBaseURL overrides the open-meteo endpoint. Tests point this
// at httptest.NewServer.URL; production should leave it default.
func WithBaseURL(u string) Option {
	return func(w *Client) { w.baseURL = u }
}

// WithClock injects a time source. Tests use this to drive cache
// TTL transitions; production keeps time.Now.
func WithClock(now func() time.Time) Option {
	return func(w *Client) { w.now = now }
}

// New constructs a Client with a fresh empty cache.
func New(opts ...Option) *Client {
	c := &Client{
		baseURL: defaultBaseURL,
		httpC:   &http.Client{Timeout: 5 * time.Second},
		now:     time.Now,
	}
	for _, o := range opts {
		o(c)
	}
	c.cache = newCache(c.now)
	return c
}

// Get returns the current snapshot for the given coordinates,
// localized into lang ("de" / "en"; empty -> German). The cache
// is consulted first; on miss the open-meteo API is called and
// the result stored. On API failure a stale snapshot is returned
// if one is available; otherwise ErrUnavailable.
//
// Saison 14-FIX07: lang was added as the trailing argument. The
// cache stores the language-neutral rawSnapshot so a second call
// in another language gets the right text from the same cache
// row (no per-language cache duplication).
func (c *Client) Get(ctx context.Context, lat, lon float64, lang string) (Snapshot, error) {
	key := newCacheKey(lat, lon)
	if raw, ok := c.cache.fresh(key); ok {
		return raw.localize(lang), nil
	}

	raw, err := c.fetch(ctx, lat, lon)
	if err == nil {
		c.cache.store(key, raw)
		return raw.localize(lang), nil
	}

	if stale, ok := c.cache.stale(key); ok {
		return stale.localize(lang), nil
	}
	return Snapshot{}, fmt.Errorf("weather: %w (last fetch error: %v)", ErrUnavailable, err)
}

// fetch issues one open-meteo call. The query asks for
// current.temperature_2m + weather_code in Europe/Berlin time, so
// the FetchedAt timestamp matches whatever the operator's site
// considers "now" without timezone gymnastics on our end.
func (c *Client) fetch(ctx context.Context, lat, lon float64) (rawSnapshot, error) {
	q := url.Values{}
	q.Set("latitude", strconv.FormatFloat(lat, 'f', 4, 64))
	q.Set("longitude", strconv.FormatFloat(lon, 'f', 4, 64))
	q.Set("current", "temperature_2m,weather_code")
	q.Set("timezone", "Europe/Berlin")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"?"+q.Encode(), nil)
	if err != nil {
		return rawSnapshot{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpC.Do(req)
	if err != nil {
		return rawSnapshot{}, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return rawSnapshot{}, fmt.Errorf("open-meteo HTTP %d: %s",
			resp.StatusCode, string(msg))
	}

	var rawJSON struct {
		Current struct {
			TemperatureC float64 `json:"temperature_2m"`
			WeatherCode  int     `json:"weather_code"`
		} `json:"current"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rawJSON); err != nil {
		return rawSnapshot{}, fmt.Errorf("decode: %w", err)
	}
	return rawSnapshot{
		TempC:       rawJSON.Current.TemperatureC,
		WeatherCode: rawJSON.Current.WeatherCode,
		FetchedAt:   c.now(),
	}, nil
}
