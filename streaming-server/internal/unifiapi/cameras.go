// Package unifiapi provides typed clients for the Protect integration
// API surface that the streaming-server uses for non-streaming
// metadata (camera enumeration today, possibly capabilities later).
//
// The actual streaming endpoint (POST .../rtsps-stream) lives in
// `internal/source/unifi` next to the RTSP-pull logic, because both
// are part of one camera lifecycle. This package isolates the
// management-side calls so the StreamBackend layer can query Protect
// without dragging the whole source machinery in.
//
// Security: the X-API-KEY is held in the [Client] and never logged.
// Error messages may quote up to 256 bytes of the response body for
// diagnostics but NEVER the request URL with credentials or the API
// key.
package unifiapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Camera is the streaming-server-facing camera record. Its JSON tags
// match the carvilon-server Naht-`streams.Camera` so the commercial-
// build wrapper can convert with a one-liner per field.
type Camera struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Online        bool   `json:"online"`
	HasPackageCam bool   `json:"has_package_cam"`
}

// Options configures a [Client].
type Options struct {
	// NVRHost is the host[:port] of the UDM running Protect, e.g.
	// "192.168.1.1". Required.
	NVRHost string

	// APIKey is the X-API-KEY value created in UniFi → Settings →
	// Integrations. Required. Never logged.
	APIKey string

	// HTTPClient overrides the HTTP client. Default: an http.Client
	// with InsecureSkipVerify and a 10 s timeout (UDM cert has no
	// IP-SAN; same posture as `internal/source/unifi`). Tests inject
	// a mock server.
	HTTPClient *http.Client
}

// Client talks to /proxy/protect/integration/v1/* on the UDM.
type Client struct {
	host   string
	apiKey string
	http   *http.Client
}

// New builds a Client. Returns an error if required Options are
// missing. Does NOT contact the NVR.
func New(opts Options) (*Client, error) {
	if opts.NVRHost == "" {
		return nil, fmt.Errorf("unifiapi: NVRHost is required")
	}
	if opts.APIKey == "" {
		return nil, fmt.Errorf("unifiapi: APIKey is required")
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Transport: &http.Transport{
				// UDM cert has no IP-SAN — same posture as the source layer.
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
			},
			Timeout: 10 * time.Second,
		}
	}
	return &Client{host: opts.NVRHost, apiKey: opts.APIKey, http: httpClient}, nil
}

// ListCameras returns all cameras the Protect controller knows about.
//
// Wire path: GET https://<host>/proxy/protect/integration/v1/cameras
// with header X-API-KEY. The response is a JSON array; only the
// fields needed by the admin dropdown are kept.
//
// Online is mapped from the Protect `state` field: "CONNECTED" is
// online, anything else (DISCONNECTED, MIGRATING, …) is offline.
func (c *Client) ListCameras(ctx context.Context) ([]Camera, error) {
	endpoint := fmt.Sprintf("https://%s/proxy/protect/integration/v1/cameras", c.host)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("unifiapi: build request: %w", err)
	}
	req.Header.Set("X-API-KEY", c.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("unifiapi: list cameras: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		// IMPORTANT: NEVER include the request URL/headers in the
		// error — headers carry the API key.
		preview := make([]byte, 256)
		n, _ := io.ReadFull(resp.Body, preview)
		return nil, fmt.Errorf("unifiapi: list cameras HTTP %d: %s",
			resp.StatusCode, preview[:n])
	}

	var raw []protectCamera
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4*1024*1024)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("unifiapi: decode camera list: %w", err)
	}

	out := make([]Camera, 0, len(raw))
	for _, r := range raw {
		out = append(out, Camera{
			ID:            r.ID,
			Name:          r.Name,
			Online:        r.State == "CONNECTED",
			HasPackageCam: r.HasPackageCamera,
		})
	}
	return out, nil
}

// protectCamera is the trimmed Protect representation. Only the
// fields the admin dropdown needs are listed; everything else in the
// response is ignored.
type protectCamera struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	State            string `json:"state"` // "CONNECTED" / "DISCONNECTED" / ...
	HasPackageCamera bool   `json:"hasPackageCamera"`
}

// --- RTSPS stream management (S20-E1) ---------------------------------------
//
// These methods manage the per-camera RTSPS pull URLs through the
// Protect integration API. They are ADDITIVE: the live RTSPS pull in
// internal/source/unifi keeps its own fetchRTSPSURL and is untouched
// in E1. Consolidating the two call sites is a later cleanup decision.
//
// Every returned URL carries a per-session auth token. Treat the whole
// RTSPSStreams value as a secret: never log it, never put it in
// committed config. The error paths redact like ListCameras (a short
// body preview only, never the request URL or headers).

// RTSPSStreams holds the per-quality RTSPS pull URLs for one camera, as
// reported by GET/POST .../rtsps-stream. An empty field means that
// quality is not currently enabled on the camera.
type RTSPSStreams struct {
	High    string
	Medium  string
	Low     string
	Package string
}

// validRTSPSQualities is the quality set the integration API accepts.
// "package" is only valid on cameras that report HasPackageCam; the API
// rejects it on others, surfaced here as a non-2xx error.
var validRTSPSQualities = map[string]bool{
	"high": true, "medium": true, "low": true, "package": true,
}

// rtspsEndpoint builds the per-camera rtsps-stream URL. The URL itself
// is non-secret (the API key travels in the X-API-KEY header); the
// per-session tokens live in the RESPONSE and must be treated as
// secrets.
func (c *Client) rtspsEndpoint(cameraID string) string {
	return fmt.Sprintf("https://%s/proxy/protect/integration/v1/cameras/%s/rtsps-stream",
		c.host, url.PathEscape(cameraID))
}

// responseError builds a redacted error for a non-2xx response. It
// quotes up to 256 bytes of the body for diagnostics but NEVER the
// request URL or headers (which carry the API key).
func responseError(op string, resp *http.Response) error {
	preview := make([]byte, 256)
	n, _ := io.ReadFull(resp.Body, preview)
	return fmt.Errorf("unifiapi: %s HTTP %d: %s", op, resp.StatusCode, preview[:n])
}

// rtspsFromMap maps the integration API's quality->URL object into the
// typed RTSPSStreams. Unknown keys are ignored; absent qualities stay "".
func rtspsFromMap(m map[string]string) RTSPSStreams {
	return RTSPSStreams{
		High:    m["high"],
		Medium:  m["medium"],
		Low:     m["low"],
		Package: m["package"],
	}
}

// GetRTSPSStream returns the RTSPS URLs currently enabled on the camera.
//
// Wire path: GET .../cameras/{id}/rtsps-stream with header X-API-KEY.
// Qualities not enabled come back as empty fields.
func (c *Client) GetRTSPSStream(ctx context.Context, cameraID string) (RTSPSStreams, error) {
	if cameraID == "" {
		return RTSPSStreams{}, fmt.Errorf("unifiapi: cameraID is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.rtspsEndpoint(cameraID), nil)
	if err != nil {
		return RTSPSStreams{}, fmt.Errorf("unifiapi: build request: %w", err)
	}
	req.Header.Set("X-API-KEY", c.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return RTSPSStreams{}, fmt.Errorf("unifiapi: get rtsps-stream: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return RTSPSStreams{}, responseError("get rtsps-stream", resp)
	}
	var raw map[string]string
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&raw); err != nil {
		return RTSPSStreams{}, fmt.Errorf("unifiapi: decode rtsps-stream: %w", err)
	}
	return rtspsFromMap(raw), nil
}

// CreateRTSPSStream enables the given quality tiers on the camera and
// returns the resulting URLs.
//
// Wire path: POST .../cameras/{id}/rtsps-stream with body
// {"qualities":[...]} and header X-API-KEY. Allowed quality values:
// high, medium, low, package. "package" only works on cameras with a
// package cam; the API rejects it otherwise (surfaced as a non-2xx
// error here).
func (c *Client) CreateRTSPSStream(ctx context.Context, cameraID string, qualities []string) (RTSPSStreams, error) {
	if cameraID == "" {
		return RTSPSStreams{}, fmt.Errorf("unifiapi: cameraID is required")
	}
	if len(qualities) == 0 {
		return RTSPSStreams{}, fmt.Errorf("unifiapi: at least one quality is required")
	}
	for _, q := range qualities {
		if !validRTSPSQualities[q] {
			return RTSPSStreams{}, fmt.Errorf("unifiapi: invalid quality %q (allowed: high, medium, low, package)", q)
		}
	}
	reqBody, err := json.Marshal(map[string]any{"qualities": qualities})
	if err != nil {
		return RTSPSStreams{}, fmt.Errorf("unifiapi: build request body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.rtspsEndpoint(cameraID), bytes.NewReader(reqBody))
	if err != nil {
		return RTSPSStreams{}, fmt.Errorf("unifiapi: build request: %w", err)
	}
	req.Header.Set("X-API-KEY", c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return RTSPSStreams{}, fmt.Errorf("unifiapi: create rtsps-stream: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return RTSPSStreams{}, responseError("create rtsps-stream", resp)
	}
	var raw map[string]string
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&raw); err != nil {
		return RTSPSStreams{}, fmt.Errorf("unifiapi: decode rtsps-stream: %w", err)
	}
	return rtspsFromMap(raw), nil
}

// DeleteRTSPSStream disables the camera's RTSPS stream(s) via
// DELETE .../cameras/{id}/rtsps-stream.
//
// Provided for the later RTSPS lifecycle (E1 only makes it available;
// nothing calls it automatically yet).
func (c *Client) DeleteRTSPSStream(ctx context.Context, cameraID string) error {
	if cameraID == "" {
		return fmt.Errorf("unifiapi: cameraID is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.rtspsEndpoint(cameraID), nil)
	if err != nil {
		return fmt.Errorf("unifiapi: build request: %w", err)
	}
	req.Header.Set("X-API-KEY", c.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("unifiapi: delete rtsps-stream: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return responseError("delete rtsps-stream", resp)
	}
	// Drain so the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	return nil
}
