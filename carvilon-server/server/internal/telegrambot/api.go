// Bot-API client: the minimal Telegram surface the bot needs (getMe,
// getUpdates, sendMessage) over plain net/http - no third-party
// Telegram library. The token rides in the request path
// (/bot<TOKEN>/<method>, the Bot API has no header auth), so EVERY
// error this client returns is sanitized before it leaves: a wrapped
// url.Error carries the full URL, and an unsanitized one would leak
// the token into logs or the admin status. Callers may log client
// errors freely; nothing here ever logs or returns the URL itself.

package telegrambot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultAPIBase is the one permitted outbound target. Tests override
// it with an httptest server; production never does.
const DefaultAPIBase = "https://api.telegram.org"

// pollTimeout is the getUpdates long-poll timeout Telegram holds the
// request open for. The HTTP client timeout must exceed it.
const pollTimeout = 25 * time.Second

// apiUser is the Bot API's User object (getMe result, message sender).
type apiUser struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

// apiChat is the Bot API's Chat object. ID is 64-bit and negative for
// groups - both are legitimate allowlist entries.
type apiChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// apiMessage is the slice of the Bot API's Message object the bot
// consumes: text only (no media per the track's scope), plus the
// sender metadata the pending list displays.
type apiMessage struct {
	Date int64    `json:"date"` // unix seconds
	Text string   `json:"text"`
	Chat apiChat  `json:"chat"`
	From *apiUser `json:"from"`
}

// apiUpdate is one getUpdates entry.
type apiUpdate struct {
	UpdateID int64       `json:"update_id"`
	Message  *apiMessage `json:"message"`
}

// apiError is a Bot-API-level failure (ok=false). RetryAfter is set on
// 429 so the sender can honour Telegram's throttle instead of hammering
// into a bot ban.
type apiError struct {
	Code       int
	Desc       string
	RetryAfter time.Duration
}

func (e *apiError) Error() string {
	return fmt.Sprintf("telegram api: %d %s", e.Code, e.Desc)
}

// apiClient calls the Bot API for one token. pollHC outlives the
// long-poll (40s > 25s hold); sendHC keeps sends snappy.
type apiClient struct {
	base   string // e.g. https://api.telegram.org
	token  string
	pollHC *http.Client
	sendHC *http.Client
}

func newAPIClient(base, token string) *apiClient {
	if base == "" {
		base = DefaultAPIBase
	}
	return &apiClient{
		base:   strings.TrimRight(base, "/"),
		token:  token,
		pollHC: &http.Client{Timeout: 40 * time.Second},
		sendHC: &http.Client{Timeout: 15 * time.Second},
	}
}

// sanitize scrubs the bot token (raw and percent-encoded) out of an
// error's text and returns an error safe for logs and the admin
// status. Every path of this client funnels errors through it. An
// *apiError keeps its type (the sender reads Code/RetryAfter via
// errors.As); everything else - notably url.Error, which carries the
// full request URL - is flattened to scrubbed text.
func (c *apiClient) sanitize(err error) error {
	if err == nil {
		return nil
	}
	var ae *apiError
	if errors.As(err, &ae) {
		return &apiError{Code: ae.Code, Desc: c.scrub(ae.Desc), RetryAfter: ae.RetryAfter}
	}
	return errors.New(c.scrub(err.Error()))
}

// scrub replaces the token (raw and percent-encoded) with "***".
func (c *apiClient) scrub(s string) string {
	if c.token == "" {
		return s
	}
	s = strings.ReplaceAll(s, c.token, "***")
	if q := url.QueryEscape(c.token); q != c.token {
		s = strings.ReplaceAll(s, q, "***")
	}
	return s
}

// call POSTs one Bot API method with a JSON body and decodes the
// result envelope into out (which may be nil). hc selects the poll or
// send client. All errors come back sanitized.
func (c *apiClient) call(ctx context.Context, hc *http.Client, method string, params, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return c.sanitize(fmt.Errorf("%s: encode: %w", method, err))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/bot"+c.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return c.sanitize(fmt.Errorf("%s: %w", method, err))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return c.sanitize(fmt.Errorf("%s: %w", method, err))
	}
	defer resp.Body.Close()

	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	// The body cap must comfortably hold a full getUpdates batch
	// (updatesLimit messages at Telegram's 4096-char max, multi-byte):
	// a batch that fails to decode would never be acked and the poller
	// would refetch the identical batch forever - a poison pill.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&envelope); err != nil {
		return c.sanitize(fmt.Errorf("%s: HTTP %d: decode: %w", method, resp.StatusCode, err))
	}
	if !envelope.OK {
		// The description is Telegram's own text ("Unauthorized",
		// "Conflict: terminated by other getUpdates request", ...) and
		// never echoes the token; sanitize anyway - belt and braces.
		return c.sanitize(&apiError{
			Code:       envelope.ErrorCode,
			Desc:       envelope.Description,
			RetryAfter: time.Duration(envelope.Parameters.RetryAfter) * time.Second,
		})
	}
	if out != nil {
		if err := json.Unmarshal(envelope.Result, out); err != nil {
			return c.sanitize(fmt.Errorf("%s: result: %w", method, err))
		}
	}
	return nil
}

// getMe validates the token and returns the bot's identity.
func (c *apiClient) getMe(ctx context.Context) (apiUser, error) {
	var u apiUser
	err := c.call(ctx, c.sendHC, "getMe", struct{}{}, &u)
	return u, err
}

// updatesLimit bounds one getUpdates batch. 30 messages at the 4096-
// char text max stay far under the response body cap - without the
// bound, Telegram sends up to 100 and a spammer could craft a batch
// the decoder rejects, which would never be acked (see call).
const updatesLimit = 30

// getUpdates long-polls for new updates from offset on. Confirmed
// updates are acked by passing lastUpdateID+1 as the next offset.
func (c *apiClient) getUpdates(ctx context.Context, offset int64) ([]apiUpdate, error) {
	params := struct {
		Offset         int64    `json:"offset,omitempty"`
		Limit          int      `json:"limit"`
		Timeout        int      `json:"timeout"`
		AllowedUpdates []string `json:"allowed_updates"`
	}{Offset: offset, Limit: updatesLimit, Timeout: int(pollTimeout / time.Second), AllowedUpdates: []string{"message"}}
	var ups []apiUpdate
	err := c.call(ctx, c.pollHC, "getUpdates", params, &ups)
	return ups, err
}

// sendMessage delivers one text message to a chat.
func (c *apiClient) sendMessage(ctx context.Context, chatID int64, text string) error {
	params := struct {
		ChatID int64  `json:"chat_id"`
		Text   string `json:"text"`
	}{ChatID: chatID, Text: text}
	return c.call(ctx, c.sendHC, "sendMessage", params, nil)
}

// asAPIError unwraps the typed Bot-API failure (sanitize preserves the
// type), so the poller can classify 401/409 and the sender can honour a
// 429's RetryAfter.
func asAPIError(err error) (*apiError, bool) {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}
