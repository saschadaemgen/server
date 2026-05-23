package streams

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// ErrNotConfigured is returned when the caller asks for a Client
// but no go2rtc base URL has been set. Surface to the admin UI as
// a "go2rtc nicht konfiguriert" banner; do not log-spam on every
// request.
var ErrNotConfigured = errors.New("streams: backend URL not configured")

// ErrProfileNotFound is returned by Get / Delete when go2rtc has
// no entry for the given name. Maps cleanly to HTTP 404 in the
// admin handlers.
var ErrProfileNotFound = errors.New("streams: profile not found")

// Client wraps the stream-server REST API at <baseURL>/api/.
//
// Public-build transitional implementation of the StreamBackend
// interface. Stream URLs (MJPEG via /api/stream.mjpeg, WebRTC via
// /offer) and the read-side surface (List via /api/profiles, Get
// via /api/profiles/{name}, Delete via /api/profiles/{name}) talk
// to the stream-server. Put currently returns ErrNotConfigured
// while GET and PUT field names on the server are still being
// unified; the admin UI is read-only against this client.
// ListCameras returns empty because the transitional client has
// no Protect connection of its own (the commercial backend wraps
// the private streaming server which does).
type Client struct {
	baseURL string
	http    *http.Client
}

// Compile-time guarantee that Client satisfies StreamBackend.
var _ StreamBackend = (*Client)(nil)

// New builds a Client against the given stream-server base URL
// (the value the operator put in CARVILON_STREAM_BACKEND_URL,
// typically http://127.0.0.1:8555). The URL must NOT include
// /api/stream.mjpeg or any other path; the client appends its
// own paths.
//
// Returns ErrNotConfigured if baseURL is empty so the caller can
// branch without a separate nil check.
func New(baseURL string) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, ErrNotConfigured
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			// REST calls finish in milliseconds in practice; cap so
			// a hung backend cannot block admin UI threads.
			Timeout: 5 * time.Second,
		},
	}, nil
}

// BaseURL exposes the configured root URL. Used by stream-proxy
// handlers to build the MJPEG passthrough URL without having to
// re-parse the env var themselves.
func (c *Client) BaseURL() string { return c.baseURL }

// Configured satisfies StreamBackend. A constructed Client is
// always wired to a backend (New rejects empty URLs).
func (c *Client) Configured() bool { return true }

// MJPEGURL renders the canonical MJPEG-passthrough URL for the
// given profile name. The handlers consume this directly with a
// plain http.Get so they can stream the body to the caller with
// http.Flusher. The stream-server exposes the MJPEG passthrough
// at /api/stream.mjpeg?src=<profile> and only accepts MJPEG
// profiles there.
func (c *Client) MJPEGURL(profile string) string {
	return c.baseURL + "/api/stream.mjpeg?src=" + url.QueryEscape(profile)
}

// WebRTCSignalURL builds the absolute URL the browser POSTs its
// SDP offer to. Form: <base>/offer?src=<profile>. The stream-
// server's /offer endpoint only accepts h264_passthrough sources;
// the resolver therefore steers TypeWeb at "intercom_web" by
// default.
//
// The /webviewer/offer proxy handler is the only caller today.
func (c *Client) WebRTCSignalURL(profile string) string {
	return c.baseURL + "/offer?src=" + url.QueryEscape(profile)
}

// List asks the stream-server for every configured profile via
// GET /api/profiles. The wire format is a JSON array of profile
// objects (camelCase keys); we sort by Name for stable admin-UI
// rendering.
//
// Saison 15-23: switched from the legacy go2rtc shape
// (map under /api/streams) to the array under /api/profiles. The
// stream-chat is still unifying the GET/PUT field names; until
// that lands the carvilon admin UI is read-only against this
// endpoint and Put returns ErrNotConfigured.
func (c *Client) List(ctx context.Context) ([]Profile, error) {
	body, err := c.do(ctx, http.MethodGet, "/api/profiles", nil)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var out []Profile
	if err := json.NewDecoder(body).Decode(&out); err != nil {
		return nil, fmt.Errorf("streams: decode list: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get returns one profile by name via GET /api/profiles/{name}.
// Maps a 404 to ErrProfileNotFound so the admin UI can render
// "schon weg" instead of a generic error.
func (c *Client) Get(ctx context.Context, name string) (Profile, error) {
	if strings.TrimSpace(name) == "" {
		return Profile{}, fmt.Errorf("streams: get: empty name")
	}
	body, err := c.do(ctx, http.MethodGet,
		"/api/profiles/"+url.PathEscape(name), nil)
	if err != nil {
		return Profile{}, err
	}
	defer body.Close()
	var p Profile
	if err := json.NewDecoder(body).Decode(&p); err != nil {
		return Profile{}, fmt.Errorf("streams: decode get: %w", err)
	}
	return p, nil
}

// Put is intentionally a stub until the stream-chat unifies the
// GET/PUT field names on the server side. Until then a best-
// effort PUT could silently lose fields under either casing, so
// the admin UI stays read-only and Put returns ErrNotConfigured.
//
// The stream-server already exposes PUT /api/profiles/{name}
// with a snake_case body today; once GET/PUT agree we wire it
// here in a follow-up briefing.
func (c *Client) Put(_ context.Context, _ Profile) error {
	return ErrNotConfigured
}

// Delete removes a profile via DELETE /api/profiles/{name}.
// Maps 404 to ErrProfileNotFound so the admin UI can render
// "schon weg" instead of a generic error. Kept functional for
// the future write-side wiring; the current admin UI does not
// call it.
func (c *Client) Delete(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("streams: delete: empty name")
	}
	body, err := c.do(ctx, http.MethodDelete,
		"/api/profiles/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	_ = body.Close()
	return nil
}

// ListCameras returns the Protect cameras the stream-server can
// reach. The transitional client has no Protect connection of
// its own; the actual camera enumeration lives in the commercial
// build. Returning an empty slice keeps the admin /a/streams
// camera dropdown harmless.
func (c *Client) ListCameras(_ context.Context) ([]Camera, error) {
	return []Camera{}, nil
}

// do issues a request to <baseURL><path> and returns the response
// body on success. Caller MUST close. Translates non-2xx into
// errors; 404 becomes ErrProfileNotFound.
func (c *Client) do(ctx context.Context, method, path string, body io.Reader) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("streams: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("streams: %s %s: %w", method, path, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, ErrProfileNotFound
	}
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("streams: %s %s: HTTP %d: %s",
			method, path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return resp.Body, nil
}

