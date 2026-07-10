// Package shelly1api talks to the local REST API of Gen1 Shelly devices
// (plain HTTP GET endpoints - /shelly, /status, /settings, /relay/N - the
// frozen legacy API) on behalf of the carvilon admin UI. It is the Gen1
// sibling of internal/shellyapi (Gen2+ JSON-RPC): same transport posture,
// different protocol - Gen1 has no /rpc, no JSON-RPC envelope, and
// authenticates with HTTP Basic (a configurable username) instead of
// digest.
//
// The client only ever speaks to the one configured LAN address, and its
// errors never carry the device address, the URL or foreign response text
// - callers log them, and the configured host must never reach a log line
// or a page (the shellyapi redaction contract, kept bit-for-bit).
//
// GET /shelly is exempt from authentication on every Gen1 firmware (the
// documented identify endpoint), so it doubles as the reachability probe
// and as the generation classifier: Gen2+ devices answer /shelly too, WITH
// a "gen" field - Gen1 devices have none.
package shelly1api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Sentinel errors callers may want to map onto fixed UI messages.
var (
	ErrUnauthorized = errors.New("shelly1api: unauthorized (check auth password)")
)

// Options configures a Client. Address is required ("ip" or "ip:port");
// Username/Password are the optional HTTP Basic credentials (Gen1 auth is
// per-device configurable; carvilon standardises on "admin", the default
// here, when it sets the credential itself).
type Options struct {
	Address  string
	Username string
	Password string
	Timeout  time.Duration
}

// Client is the per-device HTTP client. Safe for concurrent use.
type Client struct {
	addr     string
	username string
	password string
	http     *http.Client
}

// New constructs a Client for one device. A pasted URL form
// ("http://192.168.1.50/") is normalised down to the bare host so both
// spellings work; the scheme is always plain http (the only listener a
// Gen1 device has).
func New(opts Options) *Client {
	if opts.Timeout == 0 {
		opts.Timeout = 3 * time.Second
	}
	if opts.Username == "" {
		opts.Username = "admin"
	}
	addr := strings.TrimSpace(opts.Address)
	for _, scheme := range []string{"http://", "https://"} {
		addr = strings.TrimPrefix(addr, scheme)
	}
	if i := strings.IndexByte(addr, '/'); i >= 0 {
		addr = addr[:i]
	}
	return &Client{
		addr:     addr,
		username: opts.Username,
		password: opts.Password,
		http: &http.Client{
			Timeout: opts.Timeout,
			// Own transport with a nil Proxy (the shellyapi/uaapi posture):
			// the default transport consults HTTP_PROXY, and a proxy in the
			// environment would receive every request line and the Basic
			// Authorization header - this client must only ever dial the
			// configured LAN address itself.
			//
			// The explicit 2s DialContext timeout bounds the TCP connect
			// specifically, so a powered-off device fails fast instead of
			// riding the OS SYN-retransmit timeout (~2 min).
			Transport: &http.Transport{
				Proxy:       nil,
				DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
			},
			// Never follow a redirect: a compromised or mis-addressed box
			// could bounce the request (and the Basic credential) to a
			// foreign host. The Gen1 API never legitimately redirects.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Address returns the normalised device address the client dials
// ("ip" or "ip:port") - the Device Center uses it as the row identity.
func (c *Client) Address() string { return c.addr }

// maxBody caps how much of a response we read - Gen1 payloads are a few
// KB; anything bigger is not the API we expect.
const maxBody = 4 << 20

// get performs one GET request against path (e.g. "/status") with optional
// query parameters and returns the raw JSON body. Gen1 settings writes are
// side-effecting GETs with query params - the documented canonical form -
// so this one verb covers reads and writes alike. Basic auth is attached
// whenever a password is configured (Gen1 auth has no challenge dance
// worth a round-trip) - EXCEPT on /shelly: the identify endpoint is
// unauthenticated by contract, and Basic credentials ride in cleartext,
// so sending them to a probe target would hand the shared installation
// password to any LAN host that answers on the probed address before it
// proved anything about itself.
func (c *Client) get(ctx context.Context, path string, query url.Values) (json.RawMessage, error) {
	u := "http://" + c.addr + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		// A URL-parse error echoes the raw URL (the address) - drop it.
		return nil, errors.New("shelly1api: invalid device address")
	}
	req.Header.Set("Accept", "application/json")
	if c.password != "" && path != "/shelly" {
		req.SetBasicAuth(c.username, c.password)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// The transport error embeds the full URL and the dial address;
		// keep only the coarse failure kind (the redaction contract).
		return nil, errors.New("shelly1api: request failed: " + redactNetErr(err))
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, ErrUnauthorized
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, fmt.Errorf("shelly1api: http %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, errors.New("shelly1api: read: " + redactNetErr(err))
	}
	return raw, nil
}

// getJSON decodes one GET response into out (tolerantly - unknown fields
// ignored, a non-JSON body is an error).
func (c *Client) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	raw, err := c.get(ctx, path, query)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return errors.New("shelly1api: response is not the expected JSON")
	}
	return nil
}

// redactNetErr classifies a transport error without repeating any of its
// text (which carries the URL / dial address).
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
