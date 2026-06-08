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
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
