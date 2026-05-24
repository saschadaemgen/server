package streams

import (
	"bytes"
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
// /offer) plus the full CRUD surface against /api/profiles talk
// to the stream-server. The wire shape is the 11-field
// snake_case profile schema documented on Profile; GET and PUT
// share that schema verbatim so the admin UI can round-trip a
// payload it just fetched.
//
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
// objects (snake_case keys, see Profile); we sort by Name for
// stable admin-UI rendering.
//
// Saison 15-25: the stream-server unified GET / PUT field names
// at snake_case, so List, Get and Put all decode / encode the
// same Profile struct without any key translation.
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

// Get returns one profile by name. The stream-server does NOT
// expose a single-profile GET (only the list at /api/profiles
// and the write surface PUT/DELETE /api/profiles/{name}), so the
// implementation pulls the list and filters by name. ErrProfile-
// NotFound surfaces when the name is unknown so the admin UI can
// render "schon weg" instead of a generic error.
//
// Going through List is equivalent to a hypothetical single-GET
// for an admin UI's purposes: the wire shape is the same Profile
// envelope, and the list response is small enough (one entry per
// configured profile) that the round-trip cost is negligible.
func (c *Client) Get(ctx context.Context, name string) (Profile, error) {
	if strings.TrimSpace(name) == "" {
		return Profile{}, fmt.Errorf("streams: get: empty name")
	}
	profiles, err := c.List(ctx)
	if err != nil {
		return Profile{}, err
	}
	for _, p := range profiles {
		if p.Name == name {
			return p, nil
		}
	}
	return Profile{}, ErrProfileNotFound
}

// Put creates or replaces a profile via PUT /api/profiles/{name}.
// The body is the 11-field snake_case Profile envelope; the
// stream-server runs DisallowUnknownFields against it, so the
// Profile struct here is held to exactly those fields.
//
// Wire contract: 204 No Content on success, 400 with a plain-
// text reason on validation error (passed through verbatim so
// the admin UI can show the operator what the server rejected),
// 503 when the stream-server itself is down. The Name in the
// spec MUST match the path; the caller assembles that.
func (c *Client) Put(ctx context.Context, p Profile) error {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		return fmt.Errorf("streams: put: empty profile name")
	}
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("streams: encode put: %w", err)
	}
	respBody, err := c.do(ctx, http.MethodPut,
		"/api/profiles/"+url.PathEscape(name), bytes.NewReader(body))
	if err != nil {
		return err
	}
	_ = respBody.Close()
	return nil
}

// Delete removes a profile via DELETE /api/profiles/{name}.
// Maps 404 to ErrProfileNotFound so the admin UI can render
// "schon weg" instead of a generic error.
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
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
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

