// Package mqtt implements stage 6 of the UA Intercom Viewer mock
// daemon: mTLS connection to UDM's MQTT broker on port 12812 with
// a 1Hz stat-frame heartbeat carrying adopted=true, plus an RPC
// request/response loop.
//
// The heartbeat body format is saison-9 pcap-verified
// (viewer-adopt-105537.pcap). The RPC wire format is the
// protobuf-like outer wrapper documented in shared/proto.
package mqtt

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"unifix.local/mock/internal/identity"
	"unifix.local/mock/internal/state"
	"unifix.local/shared/proto"
)

const (
	heartbeatInterval = time.Second
	connectTimeout    = 15 * time.Second
	publishTimeout    = 2 * time.Second
	subscribeTimeout  = 5 * time.Second
	heartbeatLogEvery = 30 // ~1 log per 30 seconds at 1Hz
)

// Logger is the minimal logging surface this package needs.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

// RPCHandler maps an incoming RPC request to a response body.
// Saison 10 only ships DefaultHandler; method-specific handlers
// (remote_view, unlock, image capture) follow in later saisons.
type RPCHandler interface {
	Handle(path string, requestID string, body []byte) []byte
}

// DefaultHandler answers every RPC with a generic success response
// built via proto.EncodeRPCResponse. UDM is tolerant about the
// exact response form per saison-8 reverse engineering.
type DefaultHandler struct{}

// Handle returns the default success response for path/requestID.
func (DefaultHandler) Handle(path, requestID string, _ []byte) []byte {
	return proto.EncodeRPCResponse(path, requestID, "success")
}

// bootState captures values that should be fresh per daemon start
// (NOT loaded from persisted state): start_time for uptime
// reporting and a per-boot UUID emitted as the guid=... heartbeat
// field. Distinct from MockIdentity.GUID which is persistent.
type bootState struct {
	startTime int64
	bootGUID  string
}

func newBootState() (bootState, error) {
	guid, err := newUUIDv4()
	if err != nil {
		return bootState{}, err
	}
	return bootState{
		startTime: time.Now().Unix(),
		bootGUID:  guid,
	}, nil
}

func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// Client maintains the MQTT mTLS connection, publishes heartbeats
// at 1Hz, and dispatches incoming RPC requests via the configured
// handler.
type Client struct {
	identity *identity.MockIdentity
	bundle   *state.Bundle
	certDir  string
	log      Logger
	boot     bootState

	mu      sync.Mutex
	handler RPCHandler
	broker  paho.Client

	heartbeatsPublished int
	rpcsReceived        int
	rpcsAnswered        int
}

// New constructs a Client. Bundle must be from a completed
// adoption so it carries broker_address and the certs in certDir
// exist.
func New(
	id *identity.MockIdentity,
	bundle *state.Bundle,
	certDir string,
	log Logger,
) (*Client, error) {
	if id == nil {
		return nil, errors.New("mqtt: identity must not be nil")
	}
	if bundle == nil {
		return nil, errors.New("mqtt: bundle must not be nil")
	}
	if bundle.BrokerAddress == "" {
		return nil, errors.New("mqtt: bundle missing broker_address")
	}
	if bundle.ControllerID == "" {
		return nil, errors.New("mqtt: bundle missing controller_id")
	}
	if certDir == "" {
		return nil, errors.New("mqtt: certDir must not be empty")
	}
	if log == nil {
		return nil, errors.New("mqtt: logger must not be nil")
	}
	boot, err := newBootState()
	if err != nil {
		return nil, fmt.Errorf("mqtt: boot state: %w", err)
	}
	return &Client{
		identity: id,
		bundle:   bundle,
		certDir:  certDir,
		log:      log,
		boot:     boot,
		handler:  DefaultHandler{},
	}, nil
}

// SetHandler swaps in a custom RPC handler. Callable before Run.
func (c *Client) SetHandler(h RPCHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handler = h
}

// Run blocks until ctx is cancelled. Manages connect, subscribe
// (re-)wired in onConnect, the 1Hz heartbeat ticker, and clean
// disconnect on shutdown. paho handles reconnect internally.
func (c *Client) Run(ctx context.Context) error {
	tlsConfig, err := c.buildTLSConfig()
	if err != nil {
		return fmt.Errorf("mqtt tls: %w", err)
	}

	brokerURL := strings.Replace(c.bundle.BrokerAddress, "tls://", "ssl://", 1)

	opts := paho.NewClientOptions()
	opts.AddBroker(brokerURL)
	opts.SetClientID(c.identity.ID)
	opts.SetTLSConfig(tlsConfig)
	opts.SetCleanSession(true)
	opts.SetAutoReconnect(true)
	opts.SetMaxReconnectInterval(30 * time.Second)
	opts.SetConnectTimeout(connectTimeout)
	opts.SetKeepAlive(30 * time.Second)
	opts.SetOnConnectHandler(c.onConnect)
	opts.SetConnectionLostHandler(c.onConnectionLost)

	c.mu.Lock()
	c.broker = paho.NewClient(opts)
	c.mu.Unlock()

	token := c.broker.Connect()
	if !token.WaitTimeout(connectTimeout) {
		return errors.New("mqtt: connect timeout")
	}
	if err := token.Error(); err != nil {
		return fmt.Errorf("mqtt connect: %w", err)
	}
	c.log.Infof("mqtt: connected to %s", brokerURL)

	heartbeatTicker := time.NewTicker(heartbeatInterval)
	defer heartbeatTicker.Stop()
	defer c.broker.Disconnect(500)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-heartbeatTicker.C:
			c.publishHeartbeat()
		}
	}
}

func (c *Client) onConnect(_ paho.Client) {
	topic := fmt.Sprintf(proto.MQTTTopicRPCRequest,
		c.bundle.ControllerID, c.identity.ID)
	token := c.broker.Subscribe(topic, 1, c.handleRPCMessage)
	if !token.WaitTimeout(subscribeTimeout) {
		c.log.Errorf("mqtt: subscribe timeout for %s", topic)
		return
	}
	if err := token.Error(); err != nil {
		c.log.Errorf("mqtt: subscribe %s: %v", topic, err)
		return
	}
	c.log.Infof("mqtt: subscribed to %s", topic)
}

func (c *Client) onConnectionLost(_ paho.Client, err error) {
	c.log.Warnf("mqtt: connection lost: %v", err)
}

func (c *Client) publishHeartbeat() {
	topic := fmt.Sprintf(proto.MQTTTopicHeartbeat,
		c.bundle.ControllerID, c.identity.ID)
	body := buildHeartbeatBody(c.identity, c.bundle, c.boot)

	token := c.broker.Publish(topic, 0, false, body)
	if !token.WaitTimeout(publishTimeout) {
		c.log.Warnf("mqtt: heartbeat publish timeout")
		return
	}
	if err := token.Error(); err != nil {
		c.log.Warnf("mqtt: heartbeat publish failed: %v", err)
		return
	}

	c.mu.Lock()
	c.heartbeatsPublished++
	count := c.heartbeatsPublished
	c.mu.Unlock()
	if count%heartbeatLogEvery == 1 {
		c.log.Infof("mqtt: heartbeat published count=%d size=%dB", count, len(body))
	}
}

func (c *Client) handleRPCMessage(_ paho.Client, msg paho.Message) {
	c.mu.Lock()
	c.rpcsReceived++
	handler := c.handler
	c.mu.Unlock()

	req, err := proto.DecodeRPCRequest(msg.Payload())
	if err != nil {
		payload := msg.Payload()
		c.log.Warnf("mqtt: rpc decode failed topic=%s: %v", msg.Topic(), err)
		c.log.Warnf("mqtt: rpc raw payload (%d bytes): %s",
			len(payload), hex.EncodeToString(payload))
		c.log.Warnf("mqtt: rpc raw ascii preview: %q", asciiPreview(payload))
		return
	}
	c.log.Infof("mqtt: rpc received path=%s request_id=%s topic=%s",
		req.Path, req.RequestID, msg.Topic())

	responseTopic := strings.TrimSuffix(msg.Topic(), "/request") + "/response"
	responseBody := handler.Handle(req.Path, req.RequestID, req.Raw)

	token := c.broker.Publish(responseTopic, 0, false, responseBody)
	if !token.WaitTimeout(publishTimeout) {
		c.log.Warnf("mqtt: rpc response publish timeout")
		return
	}
	if err := token.Error(); err != nil {
		c.log.Warnf("mqtt: rpc response publish failed: %v", err)
		return
	}

	c.mu.Lock()
	c.rpcsAnswered++
	answered := c.rpcsAnswered
	c.mu.Unlock()
	c.log.Infof("mqtt: rpc answered path=%s total=%d", req.Path, answered)
}

// buildTLSConfig loads broker.crt + broker.key + broker_ca.crt
// from certDir for mTLS. The same UDM-cert-without-SAN quirk as
// stage 5 applies here, so verification uses the custom callback
// that pins against the CA but skips hostname matching.
func (c *Client) buildTLSConfig() (*tls.Config, error) {
	crtPath := filepath.Join(c.certDir, "broker.crt")
	keyPath := filepath.Join(c.certDir, "broker.key")
	caPath := filepath.Join(c.certDir, "broker_ca.crt")

	cert, err := tls.LoadX509KeyPair(crtPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load client cert/key: %w", err)
	}
	caBytes, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read CA %s: %w", caPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBytes) {
		return nil, fmt.Errorf("mqtt: no certificates parsed from %s", caPath)
	}

	return &tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("mqtt: peer sent no certificates")
			}
			parsed := make([]*x509.Certificate, 0, len(rawCerts))
			for _, raw := range rawCerts {
				p, err := x509.ParseCertificate(raw)
				if err != nil {
					return fmt.Errorf("parse peer cert: %w", err)
				}
				parsed = append(parsed, p)
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

// Stats returns counters for diagnostics.
func (c *Client) Stats() (heartbeats, rpcsRecv, rpcsAns int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.heartbeatsPublished, c.rpcsReceived, c.rpcsAnswered
}

// Publish sends payload to topic with QoS 0. Thread-safe; callable
// from goroutines other than Run (e.g. an HTTPS handler dispatching
// an outgoing RPC). Returns an error if the broker is not connected
// or the publish-token times out.
func (c *Client) Publish(topic string, payload []byte) error {
	c.mu.Lock()
	broker := c.broker
	c.mu.Unlock()
	if broker == nil || !broker.IsConnected() {
		return errors.New("mqtt: not connected")
	}
	token := broker.Publish(topic, 0, false, payload)
	if !token.WaitTimeout(publishTimeout) {
		return errors.New("mqtt: publish timeout")
	}
	return token.Error()
}

// asciiPreview returns the payload with non-printable bytes
// replaced by dots. Useful for spotting embedded path strings
// like "/remote_view" inside the binary frame during saison-11
// reverse engineering of the UDM request wire format.
func asciiPreview(b []byte) string {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 0x20 && c < 0x7f {
			out[i] = c
		} else {
			out[i] = '.'
		}
	}
	return string(out)
}
