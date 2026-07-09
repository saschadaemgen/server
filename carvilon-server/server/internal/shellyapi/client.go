// Package shellyapi talks to the local Gen2+ RPC API of Shelly devices
// (JSON-RPC 2.0 over HTTP, http://<ip>/rpc) on behalf of the carvilon
// admin UI. Saison 21 - Shelly Etappe 1 scope: read-only -
// Shelly.GetDeviceInfo, Shelly.GetStatus and Shelly.GetConfig fill the
// Device Center's Switches category.
//
// Shelly Etappe 3 adds a WRITE path used ONLY at device approval to
// provision the device onto the CARVILON MQTT broker and harden it
// (MQTT.SetConfig, Shelly.PutUserCA, Cloud.SetConfig, Shelly.SetAuth,
// Shelly.Reboot - see control.go). It is still not switch CONTROL: on/off
// runs over MQTT once the device is on the broker, not over this HTTP RPC.
// The client only ever speaks to the one configured LAN address.
//
// One Client per configured device address (the uaapi/protectapi
// mirror). Auth, when a device has a password set, is HTTP digest
// (RFC 7616) with the fixed Gen2 username "admin" and algorithm
// SHA-256; without a password the request simply goes out plain.
// Errors never carry the device address - callers log them, and the
// configured host must never reach a log line or a page.
package shellyapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Sentinel errors callers may want to map onto fixed UI messages.
var (
	ErrUnauthorized = errors.New("shellyapi: unauthorized (check auth password)")
)

// Options configures a Client. Address is required ("ip" or
// "ip:port"); Password is the optional digest-auth password shared by
// the installation (Gen2 username is always "admin").
type Options struct {
	Address  string
	Password string
	Timeout  time.Duration
}

// Client is the per-device HTTP client. Safe for concurrent use.
type Client struct {
	addr     string
	password string
	http     *http.Client
}

// New constructs a Client for one device. A pasted URL form
// ("http://192.168.1.50/") is normalised down to the bare host so
// both spellings work; the scheme is always plain http (the Gen2
// RPC listener).
func New(opts Options) *Client {
	if opts.Timeout == 0 {
		opts.Timeout = 3 * time.Second
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
		password: opts.Password,
		http: &http.Client{
			Timeout: opts.Timeout,
			// Own transport with a nil Proxy (the uaapi/protectapi
			// posture): the default transport consults HTTP_PROXY, and
			// a proxy in the environment would receive every request
			// line and the digest Authorization header - this client
			// must only ever dial the configured LAN address itself.
			//
			// The explicit 2s DialContext timeout bounds the TCP connect
			// specifically. A powered-off device answers no SYN, and
			// without a dial-level bound the connect falls back to the OS
			// TCP timeout (~2 min of SYN retransmits). At 2s it gives up
			// well before the 3s whole-request Timeout, so the
			// reachability poll and the Device Center probe fail fast on a
			// dead host instead of stalling the poll / the page.
			Transport: &http.Transport{
				Proxy:       nil,
				DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
			},
			// Never follow a redirect: a compromised or mis-addressed
			// box could bounce the request (and a digest response) to
			// a foreign host. The Gen2 RPC endpoint never legitimately
			// redirects; a 3xx surfaces as "shellyapi: http 3xx".
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Address returns the normalised device address the client dials
// ("ip" or "ip:port") - the Device Center uses it as the row identity.
func (c *Client) Address() string { return c.addr }

// maxBody caps how much of a response we read - Gen2 status payloads
// are a few KB; anything bigger is not the API we expect.
const maxBody = 4 << 20

// rpcEnvelope is the JSON-RPC response frame. Exactly one of result /
// error is set on a well-formed response.
type rpcEnvelope struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// rpcError is the JSON-RPC error object. Only the code is ever
// surfaced - the message text is foreign input and could carry
// device-identifying detail into a log line.
type rpcError struct {
	Code    int     `json:"code"`
	Message flexVal `json:"message"`
}

// call performs one read JSON-RPC method call (no params) and returns
// the raw result.
func (c *Client) call(ctx context.Context, method string) (json.RawMessage, error) {
	return c.callParams(ctx, method, nil)
}

// callParams performs one JSON-RPC method call with optional params
// (Saison 21 - Shelly Etappe 3: the write path for MQTT provisioning +
// hardening on approval, e.g. MQTT.SetConfig / Shelly.PutUserCA). A 401
// with a configured password triggers exactly one digest-authenticated
// retry. Errors never embed the URL, the address or foreign response
// text - only coarse, fixed failure kinds. params is nil for read calls.
func (c *Client) callParams(ctx context.Context, method string, params any) (json.RawMessage, error) {
	req := map[string]any{"id": 1, "method": method}
	if params != nil {
		req["params"] = params
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("shellyapi: marshal request: %w", err)
	}
	resp, err := c.post(ctx, body, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized && c.password != "" {
		challenge := resp.Header.Get("WWW-Authenticate")
		// Drain so the keep-alive connection can be reused for the
		// authenticated retry.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		resp.Body.Close()
		auth, derr := digestAuthorization(challenge, "admin", c.password, http.MethodPost, "/rpc")
		if derr != nil {
			return nil, ErrUnauthorized
		}
		if resp, err = c.post(ctx, body, auth); err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, ErrUnauthorized
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, fmt.Errorf("shellyapi: http %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, errors.New("shellyapi: read: " + redactNetErr(err))
	}
	var env rpcEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, errors.New("shellyapi: response is not a JSON-RPC frame")
	}
	if env.Error != nil {
		if env.Error.Code == http.StatusUnauthorized {
			return nil, ErrUnauthorized
		}
		return nil, fmt.Errorf("shellyapi: rpc error %d", env.Error.Code)
	}
	if len(bytes.TrimSpace(env.Result)) == 0 {
		return nil, errors.New("shellyapi: response carries no result")
	}
	return env.Result, nil
}

// post sends one POST /rpc request, optionally with an Authorization
// header. Transport errors come back redacted.
func (c *Client) post(ctx context.Context, body []byte, authorization string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+c.addr+"/rpc", bytes.NewReader(body))
	if err != nil {
		// A URL-parse error echoes the raw URL (the address) - drop it.
		return nil, errors.New("shellyapi: invalid device address")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// The transport error (url.Error / net.OpError) embeds the
		// full URL and the dial address; callers log these errors, and
		// the configured address must never reach a log line. Keep
		// only the coarse failure kind.
		return nil, errors.New("shellyapi: request failed: " + redactNetErr(err))
	}
	return resp, nil
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
