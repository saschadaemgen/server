// Package protectapi talks to the UniFi Protect Integration API
// (https://<udm>/proxy/protect/integration/v1/...) on behalf of the
// carvilon admin UI. Saison 21 - Protect Etappe 1 scope: a read-only
// health probe (GetMetaInfo) plus the cameras and sensors lists that
// fill the Device Center. No control endpoints - nothing in this
// package can change anything on the NVR.
//
// Auth is the `X-API-KEY` header - NOT a Bearer token; the Integration
// API differs from the UA Developer API here. Responses are plain JSON
// (no {code,msg,data} envelope); errors arrive as HTTP status codes.
// The UDM's TLS cert is self-signed without an IP SAN, so the client
// uses InsecureSkipVerify=true with an optional cert pin via
// Options.CertSHA256 - the same posture as internal/uaapi.
package protectapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Sentinel errors callers may want to map onto HTTP responses.
var (
	ErrUnauthorized = errors.New("protectapi: unauthorized (check API key)")
	ErrNotFound     = errors.New("protectapi: not found")
)

// Options configures a Client. APIKey and BaseURL are required;
// CertSHA256 is an optional pin (hex sha256 of the leaf cert).
type Options struct {
	BaseURL    string
	APIKey     string
	CertSHA256 string
	Timeout    time.Duration
}

// Client is the HTTP client. Safe for concurrent use.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// New constructs a Client with the given options. BaseURL is the
// controller root (e.g. https://192.168.1.1); a pasted full
// Integration-API base (".../proxy/protect/integration[/v1]") is
// normalised back to the root so both spellings work.
func New(opts Options) *Client {
	if opts.Timeout == 0 {
		opts.Timeout = 15 * time.Second
	}
	tlsCfg := &tls.Config{
		InsecureSkipVerify: true,
	}
	if opts.CertSHA256 != "" {
		want := strings.ToLower(opts.CertSHA256)
		tlsCfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("protectapi: no peer cert")
			}
			sum := sha256.Sum256(rawCerts[0])
			got := hex.EncodeToString(sum[:])
			if got != want {
				return fmt.Errorf("protectapi: cert pin mismatch: got %s, want %s", got, want)
			}
			return nil
		}
	}
	base := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	for _, suffix := range []string{"/proxy/protect/integration/v1", "/proxy/protect/integration"} {
		base = strings.TrimSuffix(base, suffix)
	}
	return &Client{
		baseURL: base,
		apiKey:  opts.APIKey,
		http: &http.Client{
			Timeout:   opts.Timeout,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
			// Never follow a redirect: Go strips Authorization/Cookie
			// on cross-host redirects but copies CUSTOM headers, so a
			// followed redirect (UniFi OS SSO bounce, captive portal,
			// mistyped host) would re-send X-API-KEY to a foreign
			// host. The Integration API never legitimately redirects;
			// a 3xx surfaces as "protectapi: http 3xx" instead.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// maxBody caps how much of a response we read - the cameras/sensors
// lists are small; anything bigger is not the API we expect.
const maxBody = 8 << 20

// getJSON performs a GET against the Integration API (path is the
// versioned part, e.g. "/v1/cameras") and returns the raw body.
// Status codes map onto the sentinel errors; other failures carry the
// status code only - never the URL or a response body, either of
// which could leak the configured host into logs or pages.
func (c *Client) getJSON(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/proxy/protect/integration"+path, nil)
	if err != nil {
		// A URL-parse error echoes the raw URL (the host) - drop it.
		return nil, errors.New("protectapi: invalid request URL")
	}
	req.Header.Set("X-API-KEY", c.apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		// The transport error (url.Error / net.OpError) embeds the
		// full URL and the dial address; callers log these errors, and
		// the configured host must never reach a log line. Keep only
		// the coarse failure kind.
		return nil, errors.New("protectapi: request failed: " + redactNetErr(err))
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, ErrUnauthorized
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrNotFound
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, fmt.Errorf("protectapi: http %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("protectapi: read: %w", err)
	}
	return body, nil
}

// redactNetErr classifies a transport error without repeating any of
// its text (which carries the URL / dial address).
func redactNetErr(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	return "network error"
}

// decodeArray splits a JSON array payload into its raw items,
// tolerating a {"data":[...]} wrapper in case a firmware revision
// re-wraps the lists. A present-but-null data key reads as an empty
// list (the tolerant reading); anything else is a real decode error.
func decodeArray(b []byte) ([]json.RawMessage, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(b, &items); err == nil {
		return items, nil
	}
	var wrapped map[string]json.RawMessage
	if err := json.Unmarshal(b, &wrapped); err == nil {
		if data, ok := wrapped["data"]; ok {
			if string(bytes.TrimSpace(data)) == "null" {
				return nil, nil
			}
			var inner []json.RawMessage
			if err := json.Unmarshal(data, &inner); err == nil {
				return inner, nil
			}
		}
	}
	return nil, errors.New("neither a JSON array nor a data-wrapped array")
}

// MetaInfo is the GET /v1/meta/info payload - the connection /
// health probe. Only the version is typed; the full payload stays
// in Raw for display.
type MetaInfo struct {
	ApplicationVersion flexVal `json:"applicationVersion"`

	// Raw is the full decoded object (nil when it was not an object).
	Raw map[string]any `json:"-"`
}

// GetMetaInfo probes the Integration API. A nil error means the
// configured host answered with a valid meta payload using our key.
func (c *Client) GetMetaInfo(ctx context.Context) (*MetaInfo, error) {
	body, err := c.getJSON(ctx, "/v1/meta/info")
	if err != nil {
		return nil, err
	}
	var mi MetaInfo
	if err := json.Unmarshal(body, &mi); err != nil {
		return nil, fmt.Errorf("protectapi: unmarshal meta info: %w", err)
	}
	_ = json.Unmarshal(body, &mi.Raw)
	return &mi, nil
}
