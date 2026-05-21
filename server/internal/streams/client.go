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

// Client wraps the go2rtc REST API at <baseURL>/api/.
//
// Saison 15-01: Client is now a transitional implementation of
// the StreamBackend interface. Stream URLs (MJPEG + WebRTC) and
// the List/Get/Delete surface still talk to go2rtc; Put returns
// ErrNotConfigured because profile CRUD is moving to the carvilon
// streaming server. ListCameras returns empty because go2rtc has
// no Protect connection.
type Client struct {
	baseURL string
	http    *http.Client
}

// Compile-time guarantee that Client satisfies StreamBackend.
var _ StreamBackend = (*Client)(nil)

// New builds a Client against the given go2rtc base URL (the same
// value the operator put in CARVILON_STREAM_BACKEND_URL). The URL
// must NOT include /api/stream.mjpeg or any other path; the
// client appends its own paths.
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
// http.Flusher.
//
// The path matches go2rtc's REST surface. When the proxy is moved
// to the carvilon-streaming-server (env URL flip 1984 -> 8555)
// the same path stays valid; the new server mirrors go2rtc's
// /api/stream.mjpeg endpoint on purpose so the proxy code does
// not have to change.
func (c *Client) MJPEGURL(profile string) string {
	return c.baseURL + "/api/stream.mjpeg?src=" + url.QueryEscape(profile)
}

// WebRTCSignalURL builds the absolute URL the browser POSTs its
// SDP offer to. Form: <base>/offer?src=<profile>. The carvilon-
// streaming-server exposes this endpoint; go2rtc itself does not
// (the transitional Client cannot serve real WebRTC, but the
// caller still gets a well-formed URL it can wave at the env-var
// flip to 8555).
//
// Saison 15-01: reserved seam. The /webviewer/offer proxy handler
// is the only caller today.
func (c *Client) WebRTCSignalURL(profile string) string {
	return c.baseURL + "/offer?src=" + url.QueryEscape(profile)
}

// List asks go2rtc for every configured stream. The wire format
// is {"<name>": {"producers": [...], "consumers": [...]}, ...};
// we flatten it into a sorted []Profile slice for the admin UI.
//
// Saison 15-01: the structured Profile fields (CameraID, Quality,
// Usage, Description) stay empty because go2rtc has no concept
// of them. The admin UI renders only Name + Consumers off this
// transitional response; the structured form lands once the
// carvilon-streaming-server is wired.
func (c *Client) List(ctx context.Context) ([]Profile, error) {
	body, err := c.do(ctx, http.MethodGet, "/api/streams", nil)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	raw := map[string]rawProfile{}
	if err := json.NewDecoder(body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("streams: decode list: %w", err)
	}
	out := make([]Profile, 0, len(raw))
	for name, r := range raw {
		out = append(out, r.toProfile(name))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get returns one profile by name. Maps a 404 to ErrProfileNotFound.
// Same caveat as List: structured fields stay empty.
func (c *Client) Get(ctx context.Context, name string) (Profile, error) {
	if strings.TrimSpace(name) == "" {
		return Profile{}, fmt.Errorf("streams: get: empty name")
	}
	body, err := c.do(ctx, http.MethodGet,
		"/api/streams?src="+url.QueryEscape(name), nil)
	if err != nil {
		return Profile{}, err
	}
	defer body.Close()
	var r rawProfile
	if err := json.NewDecoder(body).Decode(&r); err != nil {
		return Profile{}, fmt.Errorf("streams: decode get: %w", err)
	}
	return r.toProfile(name), nil
}

// Put is intentionally a stub during the saison-15-01 transition.
// Profile CRUD moves to the carvilon-streaming-server; the public
// build returns ErrNotConfigured so the admin UI can flash the
// migration hint without crashing.
//
// The old (name, sources) signature was replaced by Put(Profile)
// when the seam landed. go2rtc cannot represent the structured
// fields (CameraID/Quality/Usage/Description) anyway, so even a
// best-effort fallback would silently lose data; better to fail
// loudly and direct the operator at the upcoming structured form.
func (c *Client) Put(_ context.Context, _ Profile) error {
	return ErrNotConfigured
}

// Delete removes a profile. Maps 404 to ErrProfileNotFound so the
// admin UI can render "schon weg" instead of a generic error.
//
// Kept functional during the transition because pruning a stale
// go2rtc profile is still useful even after Put has moved.
func (c *Client) Delete(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("streams: delete: empty name")
	}
	body, err := c.do(ctx, http.MethodDelete,
		"/api/streams?src="+url.QueryEscape(name), nil)
	if err != nil {
		return err
	}
	_ = body.Close()
	return nil
}

// ListCameras is a no-op stub during the transition: go2rtc has
// no Protect connection. The admin UI's camera-dropdown stays
// empty until the carvilon-streaming-server takes over.
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

// rawProfile mirrors the shape go2rtc returns for one entry. We
// only keep the bits the admin UI actually renders.
type rawProfile struct {
	Producers []struct {
		URL string `json:"url"`
	} `json:"producers"`
	Consumers []json.RawMessage `json:"consumers"`
}

func (r rawProfile) toProfile(name string) Profile {
	return Profile{
		Name:      name,
		Consumers: len(r.Consumers),
	}
}
