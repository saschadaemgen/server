// Package uaapi talks to the UniFi Access Developer API on
// behalf of the unifix admin UI. Saison 12-04 scope: user CRUD
// plus a TestConnection probe. Other endpoints (doors, devices,
// webhooks, ...) will land here in later sub-briefings.
//
// The API base URL is typically https://<udm>:12445. The token
// is generated in the UniFi portal and sent as
// `Authorization: Bearer <token>` per the official API
// reference (section 2.7). UDM's TLS cert is self-signed
// without an IP SAN, so the client uses InsecureSkipVerify=true.
// An optional cert pin can be supplied via Options.CertSHA256.
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

// Result codes the UA Developer API returns in the `code`
// envelope field. The full list in section 2.4 of the official
// reference is much longer; this is the subset we map onto
// behavior. Anything else lands in a generic error with the
// code string preserved for diagnosis.
const (
	CodeSuccess             = "SUCCESS"
	CodeUnauthorized        = "CODE_UNAUTHORIZED"
	CodeAuthFailed          = "CODE_AUTH_FAILED"
	CodeAccessTokenInvalid  = "CODE_ACCESS_TOKEN_INVALID"
	CodeNotExists           = "CODE_NOT_EXISTS"
	CodeResourceNotFound    = "CODE_RESOURCE_NOT_FOUND"
	CodeUserAccountNotExist = "CODE_USER_ACCOUNT_NOT_EXIST"
	CodeUserWorkerNotExists = "CODE_USER_WORKER_NOT_EXISTS"
	CodeParamsInvalid       = "CODE_PARAMS_INVALID"
	CodeOperationForbidden  = "CODE_OPERATION_FORBIDDEN"
	CodeUserNameDuplicated  = "CODE_USER_NAME_DUPLICATED"
	CodeSystemError         = "CODE_SYSTEM_ERROR"
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

// envelope mirrors the UniFi Access Developer API response shape
// (section 2.8): {"code":"SUCCESS", "msg":"...", "data":...}.
// Note `code` is a STRING, not an int; an earlier draft of this
// client had it typed as int which broke every response decode.
type envelope struct {
	Code string          `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func (c *Client) do(req *http.Request) (*envelope, error) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if req.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("uaapi: do: %w", err)
	}
	defer resp.Body.Close()
	// Some failure modes never reach the envelope: a reverse
	// proxy can swallow the body and respond with a bare 401 or
	// 404. Map those before we try to decode JSON.
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, ErrUnauthorized
	case http.StatusNotFound:
		// Only short-circuit on 404 if we cannot decode an
		// envelope; the API itself sometimes returns 200 with
		// CODE_NOT_EXISTS. Fall through to the normal decode
		// path below.
	}
	var env envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("uaapi: decode: %w", err)
	}
	if err := mapCodeToError(env.Code, env.Msg); err != nil {
		return nil, err
	}
	return &env, nil
}

// mapCodeToError converts an API envelope code into a Go error.
// Returns nil only for CodeSuccess. Known auth and missing-
// resource codes get mapped to the sentinel errors so callers
// can switch on them with errors.Is.
func mapCodeToError(code, msg string) error {
	switch code {
	case CodeSuccess:
		return nil
	case CodeUnauthorized, CodeAuthFailed, CodeAccessTokenInvalid:
		return ErrUnauthorized
	case CodeNotExists, CodeResourceNotFound,
		CodeUserAccountNotExist, CodeUserWorkerNotExists:
		return ErrNotFound
	case CodeParamsInvalid:
		return fmt.Errorf("uaapi: invalid params: %s", msg)
	case CodeUserNameDuplicated:
		return fmt.Errorf("uaapi: user name duplicated: %s", msg)
	case CodeOperationForbidden:
		return fmt.Errorf("uaapi: operation forbidden: %s", msg)
	default:
		return fmt.Errorf("uaapi: api error code=%s msg=%s", code, msg)
	}
}
