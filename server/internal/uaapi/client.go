// Package uaapi talks to the UniFi Access Developer API on
// behalf of the unifix admin UI. Saison 12-04 scope: user CRUD
// plus a TestConnection probe. Other endpoints (doors, devices,
// webhooks, ...) will land here in later sub-briefings.
//
// The API base URL is typically https://<udm>:12445 and the
// token is the X-API-KEY header value generated in the UniFi
// portal. UDM's TLS cert is self-signed without an IP SAN, so
// the client uses InsecureSkipVerify=true. An optional cert pin
// could be added later via VerifyPeerCertificate; the operator
// can wire that through Options.CertSHA256 if they want.
package uaapi

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Sentinel errors callers may want to map onto HTTP responses.
var (
	ErrUnauthorized = errors.New("uaapi: unauthorized (check API token)")
	ErrNotFound     = errors.New("uaapi: not found")
)

// Options configures a Client. Token and BaseURL are required;
// CertSHA256 is optional pin (hex sha256 of the leaf cert).
type Options struct {
	BaseURL    string
	Token      string
	CertSHA256 string
	Timeout    time.Duration
}

// Client is the HTTP client. Safe for concurrent use.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New constructs a Client with the given options.
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
				return errors.New("uaapi: no peer cert")
			}
			sum := sha256.Sum256(rawCerts[0])
			got := hex.EncodeToString(sum[:])
			if got != want {
				return fmt.Errorf("uaapi: cert pin mismatch: got %s, want %s", got, want)
			}
			return nil
		}
	}
	return &Client{
		baseURL: strings.TrimRight(opts.BaseURL, "/"),
		token:   opts.Token,
		http: &http.Client{
			Timeout:   opts.Timeout,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
	}
}

// envelope mirrors the UniFi Access Developer API response shape:
// {"code":0, "msg":"...", "data": ...}
type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func (c *Client) do(req *http.Request) (*envelope, error) {
	req.Header.Set("X-API-KEY", c.token)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("uaapi: do: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, ErrUnauthorized
	case http.StatusNotFound:
		return nil, ErrNotFound
	}
	var env envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("uaapi: decode: %w", err)
	}
	if resp.StatusCode >= 400 || env.Code != 0 {
		return nil, fmt.Errorf("uaapi: api error: status=%d code=%d msg=%q",
			resp.StatusCode, env.Code, env.Msg)
	}
	return &env, nil
}
