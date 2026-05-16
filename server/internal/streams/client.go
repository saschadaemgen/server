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
type Client struct {
	baseURL string
	http    *http.Client
}

// New builds a Client against the given go2rtc base URL (the same
// value the operator put in UNIFIX_STREAM_BACKEND_URL). The URL
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
			// a hung go2rtc cannot block admin UI threads.
			Timeout: 5 * time.Second,
		},
	}, nil
}

// BaseURL exposes the configured root URL. Used by stream-proxy
// handlers to build the MJPEG passthrough URL without having to
// re-parse UNIFIX_STREAM_BACKEND_URL themselves.
func (c *Client) BaseURL() string { return c.baseURL }

// MJPEGURL renders the canonical MJPEG-passthrough URL for the
// given profile name. The handlers consume this directly with a
// plain http.Get so they can stream the body to the caller with
// http.Flusher.
func (c *Client) MJPEGURL(profile string) string {
	return c.baseURL + "/api/stream.mjpeg?src=" + url.QueryEscape(profile)
}

// List asks go2rtc for every configured stream. The wire format
// is {"<name>": {"producers": [...], "consumers": [...]}, ...};
// we flatten it into a sorted []Profile slice for the admin UI.
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

// Put creates or replaces a profile. Multiple source URLs are
// allowed; the admin UI passes them as a slice. Empty sources is
// rejected by go2rtc with a 400; we mirror that.
//
// go2rtc accepts the source URL as the value of repeated src
// query parameters (?src=<url>&src=<url>...). The body is empty.
// On success it returns 200 with an empty body.
func (c *Client) Put(ctx context.Context, name string, sources []string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("streams: put: empty name")
	}
	if len(sources) == 0 {
		return fmt.Errorf("streams: put: at least one source URL required")
	}
	q := url.Values{}
	q.Set("name", name)
	for _, src := range sources {
		s := strings.TrimSpace(src)
		if s == "" {
			continue
		}
		q.Add("src", s)
	}
	body, err := c.do(ctx, http.MethodPut, "/api/streams?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	_ = body.Close()
	return nil
}

// Delete removes a profile. Maps 404 to ErrProfileNotFound so the
// admin UI can render "schon weg" instead of a generic error.
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
	srcs := make([]string, 0, len(r.Producers))
	for _, p := range r.Producers {
		if p.URL != "" {
			srcs = append(srcs, p.URL)
		}
	}
	return Profile{
		Name:      name,
		Sources:   srcs,
		Consumers: len(r.Consumers),
	}
}
