// Package websocket implements stage 5 of the UA Intercom Viewer
// mock daemon: a WebSocket client that connects to UDM's
// notification endpoint after adoption, authenticates via JWT,
// and processes the incoming Hello-heartbeat plus access.* event
// stream. Reconnects with exponential backoff on disconnects.
package websocket

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"

	"carvilon.local/mock/internal/crypto"
	"carvilon.local/mock/internal/identity"
	"carvilon.local/mock/internal/state"
)

const (
	notificationURL = "wss://192.168.1.1:12443/api/v2/ws/notification"
	initialBackoff  = time.Second
	maxBackoff      = 30 * time.Second
	helloLogEveryN  = 12 // ~1 log per minute at 5s heartbeat
)

// Logger is the minimal logging surface this package needs.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

// Client maintains the WebSocket connection to UDM's notification
// endpoint and counts incoming heartbeats and events.
type Client struct {
	identity   *identity.MockIdentity
	bundle     *state.Bundle
	caCertPath string
	log        Logger

	mu           sync.Mutex
	connectCount int
	messageCount int
	helloCount   int
	eventCount   int
}

// New constructs a Client. The bundle must come from a completed
// adoption (provides the CA used for TLS verification, persisted
// at caCertPath = state/<mock-id>/certs/broker_ca.crt).
func New(id *identity.MockIdentity, bundle *state.Bundle, caCertPath string, log Logger) (*Client, error) {
	if id == nil {
		return nil, errors.New("ws: identity must not be nil")
	}
	if bundle == nil {
		return nil, errors.New("ws: bundle must not be nil")
	}
	if caCertPath == "" {
		return nil, errors.New("ws: caCertPath must not be empty")
	}
	if log == nil {
		return nil, errors.New("ws: logger must not be nil")
	}
	return &Client{
		identity:   id,
		bundle:     bundle,
		caCertPath: caCertPath,
		log:        log,
	}, nil
}

// Run blocks until ctx is cancelled. Manages the connect loop
// with exponential backoff between attempts (1s up to 30s).
func (c *Client) Run(ctx context.Context) error {
	backoff := initialBackoff
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := c.connectAndServe(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}

		c.log.Warnf("ws: disconnected: %v (reconnecting in %s)", err, backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (c *Client) connectAndServe(ctx context.Context) error {
	tlsConfig, err := c.buildTLSConfig()
	if err != nil {
		return fmt.Errorf("tls config: %w", err)
	}

	token, err := crypto.SignJWT(c.identity.ID)
	if err != nil {
		return fmt.Errorf("jwt sign: %w", err)
	}

	dialOpts := &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": []string{"Bearer " + token},
		},
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: tlsConfig,
			},
		},
	}

	conn, _, err := websocket.Dial(ctx, notificationURL, dialOpts)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close(websocket.StatusInternalError, "client closing")

	c.mu.Lock()
	c.connectCount++
	connNum := c.connectCount
	c.mu.Unlock()
	c.log.Infof("ws: connected #%d to %s", connNum, notificationURL)

	// Permit larger inbound messages than the 32 KB default.
	conn.SetReadLimit(1 << 20)

	for {
		if ctx.Err() != nil {
			_ = conn.Close(websocket.StatusNormalClosure, "ctx done")
			return ctx.Err()
		}
		_, data, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read: %w", err)
		}
		c.dispatchFrame(data)
	}
}

// dispatchFrame parses one inbound WebSocket frame. UDM sends both
// plain JSON-string heartbeats ("Hello") and JSON-object events,
// so we decode into any and type-switch.
func (c *Client) dispatchFrame(data []byte) {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		c.handleStringMessage(string(data))
		return
	}
	switch v := raw.(type) {
	case string:
		c.handleStringMessage(v)
	case map[string]any:
		c.handleMessage(v)
	default:
		c.mu.Lock()
		c.messageCount++
		c.eventCount++
		c.mu.Unlock()
		c.log.Infof("ws: event of non-map type (event_count=%d)", c.eventCount)
	}
}

// handleStringMessage counts plain-string frames. UDM's heartbeat
// is the literal string "Hello"; anything else is treated as a
// generic event.
func (c *Client) handleStringMessage(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messageCount++
	if strings.Contains(strings.ToLower(s), "hello") {
		c.helloCount++
		if c.helloCount%helloLogEveryN == 1 {
			c.log.Infof("ws: hello heartbeat (count=%d)", c.helloCount)
		}
		return
	}
	c.eventCount++
	c.log.Infof("ws: event string=%q (event_count=%d)", s, c.eventCount)
}

// buildTLSConfig pins the connection against the UniFi-Access-CA
// persisted from the adoption bundle. UDM's server cert at :12443
// has CN=unifi.local but no Subject Alternative Names, which Go's
// strict verifier rejects since 1.15. We therefore disable the
// stdlib hostname check via InsecureSkipVerify and re-implement
// chain validation in VerifyPeerCertificate so the CA pin still
// applies.
func (c *Client) buildTLSConfig() (*tls.Config, error) {
	caBytes, err := os.ReadFile(c.caCertPath)
	if err != nil {
		return nil, fmt.Errorf("read CA %s: %w", c.caCertPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBytes) {
		return nil, fmt.Errorf("ws: no certificates parsed from %s", c.caCertPath)
	}
	return &tls.Config{
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("ws: peer sent no certificates")
			}
			parsed := make([]*x509.Certificate, 0, len(rawCerts))
			for _, raw := range rawCerts {
				cert, err := x509.ParseCertificate(raw)
				if err != nil {
					return fmt.Errorf("parse peer cert: %w", err)
				}
				parsed = append(parsed, cert)
			}
			intermediates := x509.NewCertPool()
			for _, cert := range parsed[1:] {
				intermediates.AddCert(cert)
			}
			_, err := parsed[0].Verify(x509.VerifyOptions{
				Roots:         pool,
				Intermediates: intermediates,
			})
			return err
		},
		MinVersion: tls.VersionTLS12,
	}, nil
}

func (c *Client) handleMessage(raw map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.messageCount++

	msgType, _ := raw["msg_type"].(string)
	if msgType == "" {
		msgType, _ = raw["type"].(string)
	}

	if strings.Contains(strings.ToLower(msgType), "hello") {
		c.helloCount++
		if c.helloCount%helloLogEveryN == 1 {
			c.log.Infof("ws: hello heartbeat (count=%d)", c.helloCount)
		}
		return
	}

	c.eventCount++
	c.log.Infof("ws: event msg_type=%q (event_count=%d)", msgType, c.eventCount)
}

// Stats returns connection and message counters for diagnostics.
func (c *Client) Stats() (connects, messages, hellos, events int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connectCount, c.messageCount, c.helloCount, c.eventCount
}
