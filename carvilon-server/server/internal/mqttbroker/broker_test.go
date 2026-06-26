package mqttbroker

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"

	"carvilon.local/server/internal/db"
	"carvilon.local/server/internal/mqttstore"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func newStore(t *testing.T) *mqttstore.Store {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return mqttstore.New(d.DB, func(context.Context) (string, error) { return "pep", nil })
}

func startManagerC(t *testing.T, store *mqttstore.Store, console *Console) (*Manager, int, int) {
	t.Helper()
	tcpPort, tlsPort := freePort(t), freePort(t)
	m := New(store, console, discardLogger(), t.TempDir(), Settings{
		Enabled: true,
		LANHost: "127.0.0.1",
		TCPPort: tcpPort,
		TLSHost: "127.0.0.1",
		TLSPort: tlsPort,
	})
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(m.Stop)
	time.Sleep(100 * time.Millisecond)
	return m, tcpPort, tlsPort
}

func startManager(t *testing.T, store *mqttstore.Store) (*Manager, int, int) {
	t.Helper()
	tcpPort, tlsPort := freePort(t), freePort(t)
	m := New(store, NewConsole(100), discardLogger(), t.TempDir(), Settings{
		Enabled: true,
		LANHost: "127.0.0.1",
		TCPPort: tcpPort,
		TLSHost: "127.0.0.1",
		TLSPort: tlsPort,
	})
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(m.Stop)
	// give the listeners a beat to accept.
	time.Sleep(100 * time.Millisecond)
	return m, tcpPort, tlsPort
}

func TestBrokerAuthPlaintext(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if err := store.CreateDevice(ctx, "dev1", "password123", ""); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	_, tcpPort, _ := startManager(t, store)

	// valid credentials -> accepted
	c := connect(t, fmt.Sprintf("tcp://127.0.0.1:%d", tcpPort), "dev1", "password123", nil)
	if c == nil {
		t.Fatal("valid credentials should connect")
	}
	c.Disconnect(100)

	// invalid password -> refused
	if bad := connect(t, fmt.Sprintf("tcp://127.0.0.1:%d", tcpPort), "dev1", "wrong", nil); bad != nil {
		bad.Disconnect(100)
		t.Fatal("invalid password must be refused")
	}
	// unknown user -> refused
	if bad := connect(t, fmt.Sprintf("tcp://127.0.0.1:%d", tcpPort), "ghost", "password123", nil); bad != nil {
		bad.Disconnect(100)
		t.Fatal("unknown user must be refused")
	}
}

func TestBrokerAuthTLS(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if err := store.CreateDevice(ctx, "dev1", "password123", ""); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	_, _, tlsPort := startManager(t, store)

	tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test pins nothing
	c := connect(t, fmt.Sprintf("ssl://127.0.0.1:%d", tlsPort), "dev1", "password123", tlsCfg)
	if c == nil {
		t.Fatal("valid credentials over TLS should connect")
	}
	c.Disconnect(100)

	if bad := connect(t, fmt.Sprintf("ssl://127.0.0.1:%d", tlsPort), "dev1", "nope", tlsCfg); bad != nil {
		bad.Disconnect(100)
		t.Fatal("bad password over TLS must be refused (plaintext is not a bypass, and neither is TLS)")
	}
}

func TestBrokerPublishDeliveryWithinSubtree(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if err := store.CreateDevice(ctx, "dev1", "password123", ""); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	_, tcpPort, _ := startManager(t, store)
	url := fmt.Sprintf("tcp://127.0.0.1:%d", tcpPort)

	got := make(chan string, 1)
	sub := connect(t, url, "dev1", "password123", nil)
	if sub == nil {
		t.Fatal("subscriber connect failed")
	}
	defer sub.Disconnect(100)
	tok := sub.Subscribe("carvilon/dev1/state", 0, func(_ paho.Client, msg paho.Message) {
		got <- string(msg.Payload())
	})
	if !tok.WaitTimeout(2*time.Second) || tok.Error() != nil {
		t.Fatalf("subscribe (own subtree) should succeed: %v", tok.Error())
	}

	pub := connect(t, url, "dev1", "password123", nil)
	if pub == nil {
		t.Fatal("publisher connect failed")
	}
	defer pub.Disconnect(100)
	pub.Publish("carvilon/dev1/state", 0, false, "on").WaitTimeout(2 * time.Second)

	select {
	case p := <-got:
		if p != "on" {
			t.Fatalf("payload = %q, want on", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("message within own subtree was not delivered")
	}
}

// TestBrokerConsoleReceivesRealTraffic closes the full loop: a real
// client connect + publish flows through the mochi console hook into
// the Console hub (what the live SSE console renders).
func TestBrokerConsoleReceivesRealTraffic(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if err := store.CreateDevice(ctx, "dev1", "password123", ""); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	console := NewConsole(100)
	events, cancel := console.Subscribe(64)
	defer cancel()

	_, tcpPort, _ := startManagerC(t, store, console)
	c := connect(t, fmt.Sprintf("tcp://127.0.0.1:%d", tcpPort), "dev1", "password123", nil)
	if c == nil {
		t.Fatal("connect failed")
	}
	defer c.Disconnect(100)
	c.Publish("carvilon/dev1/state", 0, false, "on").WaitTimeout(2 * time.Second)

	sawConnect, sawPublish := false, false
	deadline := time.After(3 * time.Second)
	for !sawPublish {
		select {
		case ev := <-events:
			switch ev.Kind {
			case "connect", "auth":
				if ev.User == "dev1" {
					sawConnect = true
				}
			case "publish":
				if ev.Topic == "carvilon/dev1/state" && ev.User == "dev1" {
					sawPublish = true
				}
			}
		case <-deadline:
			t.Fatalf("console did not observe real traffic (connect=%v publish=%v)", sawConnect, sawPublish)
		}
	}
	if !sawConnect {
		t.Error("console missed the connect/auth event")
	}
}

// TestACLHookDeny exercises the enforcement glue directly: the hook
// reads the snapshot and denies an out-of-subtree publish.
func TestACLHookDeny(t *testing.T) {
	az := &mqttstore.Authz{Devices: map[string]mqttstore.DeviceAuthz{
		"dev1": {Rules: nil},
	}}
	h := &authzHook{}
	h.setAuthz(az)

	cl := &mqtt.Client{}
	cl.Properties.Username = []byte("dev1")

	if !h.OnACLCheck(cl, "carvilon/dev1/x", true) {
		t.Error("own-subtree publish should be allowed")
	}
	if h.OnACLCheck(cl, "forbidden/x", true) {
		t.Error("out-of-subtree publish must be denied")
	}
	if h.OnACLCheck(cl, "forbidden/#", false) {
		t.Error("out-of-subtree subscribe must be denied")
	}

	// auth glue
	connPk := packets.Packet{}
	connPk.Connect.Username = []byte("dev1")
	// no device with a real hash here, so this verifies the unknown
	// path returns false without panicking.
	if h.OnConnectAuthenticate(&mqtt.Client{}, connPk) {
		t.Error("authenticate against snapshot without a hash must fail closed")
	}
}

func connect(t *testing.T, url, user, pass string, tlsCfg *tls.Config) paho.Client {
	t.Helper()
	opts := paho.NewClientOptions().
		AddBroker(url).
		SetClientID(fmt.Sprintf("test-%s-%d", user, time.Now().UnixNano())).
		SetUsername(user).
		SetPassword(pass).
		SetConnectTimeout(2 * time.Second).
		SetConnectRetry(false).
		SetAutoReconnect(false)
	if tlsCfg != nil {
		opts.SetTLSConfig(tlsCfg)
	}
	c := paho.NewClient(opts)
	tok := c.Connect()
	if !tok.WaitTimeout(3 * time.Second) {
		return nil
	}
	if tok.Error() != nil {
		return nil
	}
	return c
}
