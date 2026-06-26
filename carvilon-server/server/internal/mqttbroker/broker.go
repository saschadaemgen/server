// Package mqttbroker embeds the mochi-mqtt/server/v2 broker (MIT) as
// an on/off Carvilon subsystem with device authentication, per-topic
// ACLs, and TLS. It is network-exposed, so security is wired in from
// the start: every CONNECT is authenticated and every publish/
// subscribe is ACL-checked on BOTH the plaintext-LAN and the TLS
// transport. The Manager owns the broker lifecycle and supports live
// start/stop/reconfigure so the admin on/off toggle and port/TLS
// changes take effect without a process restart; credential and ACL
// edits are applied by swapping an in-memory authz snapshot (see
// hooks.go), again restart-free.
package mqttbroker

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"sync"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/listeners"

	"carvilon.local/server/internal/mqttstore"
)

// Settings are the runtime-tunable broker parameters. They are
// persisted by the admin layer (platform_config) and handed to the
// Manager; the Manager itself does not read or write config storage.
type Settings struct {
	Enabled bool
	// LANHost is the bind host for the PLAINTEXT listener. It must be
	// a LAN address (never 0.0.0.0): plaintext MQTT stays inside the
	// LAN and is never exposed to the internet. Empty falls back to
	// loopback (127.0.0.1) so a misconfiguration cannot leak.
	LANHost string
	TCPPort int // plaintext port, default 1883
	// TLSHost is the bind host for the TLS listener. Empty binds all
	// interfaces (0.0.0.0): TLS is authenticated+encrypted and may be
	// reached over untrusted networks.
	TLSHost string
	TLSPort int // TLS port, default 8883
	// CertFile/KeyFile are operator-provided PEM paths. When empty, a
	// self-signed cert is generated once under the state dir.
	CertFile string
	KeyFile  string
}

// Status is a read snapshot for the admin UI.
type Status struct {
	Enabled    bool
	Running    bool
	TCPAddr    string
	TLSAddr    string
	CertSource string
	Error      string
}

// Manager owns the broker lifecycle.
type Manager struct {
	store    *mqttstore.Store
	console  *Console
	log      *slog.Logger
	stateDir string // base dir for the generated self-signed cert

	mu       sync.Mutex
	settings Settings
	running  bool
	srv      *mqtt.Server
	authz    *authzHook
	tcpAddr  string
	tlsAddr  string
	certSrc  string
	lastErr  string
}

// New builds a Manager. stateDir is where the self-signed TLS cert is
// generated when no operator cert is configured.
func New(store *mqttstore.Store, console *Console, log *slog.Logger, stateDir string, settings Settings) *Manager {
	return &Manager{
		store:    store,
		console:  console,
		log:      log.With("component", "mqtt-broker"),
		stateDir: stateDir,
		settings: settings,
	}
}

// Console exposes the live-traffic hub for the SSE handler.
func (m *Manager) Console() *Console { return m.console }

// SettingsSnapshot returns the current settings.
func (m *Manager) SettingsSnapshot() Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settings
}

// Status returns a snapshot for the admin UI.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Status{
		Enabled:    m.settings.Enabled,
		Running:    m.running,
		TCPAddr:    m.tcpAddr,
		TLSAddr:    m.tlsAddr,
		CertSource: m.certSrc,
		Error:      m.lastErr,
	}
}

// Start brings the broker up if enabled. A disabled broker is a
// no-op success. A bind/cert failure is recorded and returned but
// does not panic the host process.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startLocked(ctx)
}

// Stop tears the broker down. Safe to call when not running.
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked()
}

// Reconfigure applies new settings, restarting the listeners. The
// admin layer persists the settings first, then calls this.
func (m *Manager) Reconfigure(ctx context.Context, s Settings) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked()
	m.settings = s
	return m.startLocked(ctx)
}

// ReloadAuthz swaps in a fresh credential/ACL snapshot without
// touching the listeners. Called after any device or ACL change.
func (m *Manager) ReloadAuthz(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.running || m.authz == nil {
		return nil // will be loaded on next Start
	}
	az, err := m.store.LoadAuthz(ctx)
	if err != nil {
		return err
	}
	m.authz.setAuthz(az)
	return nil
}

func (m *Manager) stopLocked() {
	if m.srv != nil {
		_ = m.srv.Close()
		m.srv = nil
	}
	m.authz = nil
	m.running = false
	m.tcpAddr = ""
	m.tlsAddr = ""
	m.certSrc = ""
}

func (m *Manager) startLocked(ctx context.Context) error {
	m.lastErr = ""
	if !m.settings.Enabled {
		m.running = false
		return nil
	}

	az, err := m.store.LoadAuthz(ctx)
	if err != nil {
		return m.fail(fmt.Errorf("load authz: %w", err))
	}

	authz := &authzHook{}
	authz.setAuthz(az)
	authz.onConnect = func(user string) {
		m.console.Publish(Event{Time: nowMillis(), Kind: "auth", User: user, Detail: "accepted"})
		// best-effort last_connect bookkeeping, off the broker path
		go func() { _ = m.store.TouchConnect(context.Background(), user) }()
	}

	srv := mqtt.New(&mqtt.Options{
		InlineClient: false,
		Logger:       m.log,
	})
	if err := srv.AddHook(authz, nil); err != nil {
		return m.fail(fmt.Errorf("add authz hook: %w", err))
	}
	if err := srv.AddHook(&consoleHook{console: m.console}, nil); err != nil {
		return m.fail(fmt.Errorf("add console hook: %w", err))
	}

	// --- plaintext listener: LAN-only, never 0.0.0.0 ---
	lanHost := m.settings.LANHost
	if lanHost == "" {
		lanHost = "127.0.0.1"
		m.log.Warn("mqtt plaintext bind host empty; falling back to loopback (LAN clients will not reach the broker)")
	}
	tcpAddr := net.JoinHostPort(lanHost, strconv.Itoa(m.settings.TCPPort))
	tcp := listeners.NewTCP(listeners.Config{Type: listeners.TypeTCP, ID: "lan-tcp", Address: tcpAddr})
	if err := srv.AddListener(tcp); err != nil {
		_ = srv.Close()
		return m.fail(fmt.Errorf("bind plaintext %s: %w", tcpAddr, err))
	}

	// --- TLS listener ---
	certFile, keyFile, certSrc, err := m.resolveCert(lanHost)
	if err != nil {
		_ = srv.Close()
		return m.fail(fmt.Errorf("tls cert: %w", err))
	}
	tlsCfg, err := loadTLSConfig(certFile, keyFile)
	if err != nil {
		_ = srv.Close()
		return m.fail(fmt.Errorf("load tls keypair: %w", err))
	}
	tlsAddr := net.JoinHostPort(m.settings.TLSHost, strconv.Itoa(m.settings.TLSPort))
	tlsLn := listeners.NewTCP(listeners.Config{Type: listeners.TypeTCP, ID: "tls", Address: tlsAddr, TLSConfig: tlsCfg})
	if err := srv.AddListener(tlsLn); err != nil {
		_ = srv.Close()
		return m.fail(fmt.Errorf("bind tls %s: %w", tlsAddr, err))
	}

	if err := srv.Serve(); err != nil {
		_ = srv.Close()
		return m.fail(fmt.Errorf("serve: %w", err))
	}

	m.srv = srv
	m.authz = authz
	m.running = true
	m.tcpAddr = tcpAddr
	m.tlsAddr = tlsAddr
	m.certSrc = certSrc
	m.log.Info("mqtt broker started", "plaintext", tcpAddr, "tls", tlsAddr, "cert", certSrc)
	return nil
}

func (m *Manager) fail(err error) error {
	m.lastErr = err.Error()
	m.running = false
	m.log.Error("mqtt broker start failed", "err", err)
	return err
}

// resolveCert returns the cert+key paths to use and a human label for
// the source. An operator pair is used verbatim; otherwise a
// self-signed pair is generated once under <stateDir>/mqtt/.
func (m *Manager) resolveCert(lanHost string) (certFile, keyFile, source string, err error) {
	if m.settings.CertFile != "" && m.settings.KeyFile != "" {
		return m.settings.CertFile, m.settings.KeyFile, "operator", nil
	}
	dir := filepath.Join(m.stateDir, "mqtt")
	certFile = filepath.Join(dir, "tls.crt")
	keyFile = filepath.Join(dir, "tls.key")
	hosts := []string{lanHost, "localhost", "127.0.0.1"}
	if m.settings.TLSHost != "" {
		hosts = append(hosts, m.settings.TLSHost)
	}
	generated, err := ensureSelfSigned(certFile, keyFile, hosts)
	if err != nil {
		return "", "", "", err
	}
	if generated {
		return certFile, keyFile, "self-signed (generated)", nil
	}
	return certFile, keyFile, "self-signed", nil
}

func loadTLSConfig(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// ErrDisabled is returned by callers that expect a running broker.
var ErrDisabled = errors.New("mqttbroker: broker disabled")
