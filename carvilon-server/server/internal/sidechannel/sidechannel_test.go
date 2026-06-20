package sidechannel

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"

	"carvilon.local/server/internal/streampublish"
	"carvilon.local/server/internal/streamstore"
	"carvilon.local/server/internal/turnstore"
)

// --- test helpers ---------------------------------------------------

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type certPaths struct {
	caCrt, serverCrt, serverKey, clientCrt, clientKey string
}

func serial(t *testing.T) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	return n
}

func writeCertPEM(t *testing.T, path string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatalf("write cert %s: %v", path, err)
	}
}

func writeKeyPEM(t *testing.T, path string, key *ecdsa.PrivateKey) {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write key %s: %v", path, err)
	}
}

// newCA returns a fresh self-signed CA (key + parsed cert + DER).
func newCA(t *testing.T) (*ecdsa.PrivateKey, *x509.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("ca cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}
	return key, cert, der
}

// leaf signs a leaf cert (server or client) with the given CA.
func leaf(t *testing.T, caKey *ecdsa.PrivateKey, caCert *x509.Certificate, cn string, eku x509.ExtKeyUsage, ips []net.IP) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(t),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{eku},
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("leaf cert: %v", err)
	}
	return key, der
}

// genCerts writes a CA + server(IP SAN 127.0.0.1) + client into dir.
func genCerts(t *testing.T, dir string) certPaths {
	t.Helper()
	caKey, caCert, caDER := newCA(t)
	loop := []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback}
	srvKey, srvDER := leaf(t, caKey, caCert, "test-server", x509.ExtKeyUsageServerAuth, loop)
	cliKey, cliDER := leaf(t, caKey, caCert, "test-client", x509.ExtKeyUsageClientAuth, nil)

	p := certPaths{
		caCrt:     filepath.Join(dir, "ca.crt"),
		serverCrt: filepath.Join(dir, "server.crt"),
		serverKey: filepath.Join(dir, "server.key"),
		clientCrt: filepath.Join(dir, "client.crt"),
		clientKey: filepath.Join(dir, "client.key"),
	}
	writeCertPEM(t, p.caCrt, caDER)
	writeCertPEM(t, p.serverCrt, srvDER)
	writeKeyPEM(t, p.serverKey, srvKey)
	writeCertPEM(t, p.clientCrt, cliDER)
	writeKeyPEM(t, p.clientKey, cliKey)
	return p
}

// startServer binds a 127.0.0.1:0 listener and runs a sidechannel
// server on it, returning the server, its dial URL, and a stop func.
func startServer(t *testing.T, ctx context.Context, p certPaths) (*Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv, err := NewServer(ServerOptions{
		Listener:   ln,
		CACertPath: p.caCrt,
		ServerCert: p.serverCrt,
		ServerKey:  p.serverKey,
		Log:        quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go func() { _ = srv.Run(ctx) }()
	return srv, "wss://" + ln.Addr().String() + "/sidechannel"
}

func waitFor(t *testing.T, cond func() bool, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", d)
}

// --- tests ----------------------------------------------------------

func TestSidechannel_PingPongRoundtrip(t *testing.T) {
	dir := t.TempDir()
	p := genCerts(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, url := startServer(t, ctx, p)

	pong := make(chan struct{}, 16)
	cli, err := NewClient(ClientOptions{
		URL:            url,
		CACertPath:     p.caCrt,
		ClientCert:     p.clientCrt,
		ClientKey:      p.clientKey,
		Log:            quietLogger(),
		PingInterval:   50 * time.Millisecond,
		InitialBackoff: 50 * time.Millisecond,
		OnPong: func() {
			select {
			case pong <- struct{}{}:
			default:
			}
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	go func() { _ = cli.Run(ctx) }()

	select {
	case <-pong:
	case <-time.After(5 * time.Second):
		t.Fatal("no pong received within 5s")
	}
	if got := srv.ConnCount(); got != 1 {
		t.Errorf("server ConnCount = %d, want 1", got)
	}
}

func TestSidechannel_RejectsClientWithoutCert(t *testing.T) {
	dir := t.TempDir()
	p := genCerts(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, url := startServer(t, ctx, p)

	// Trusts the server CA but presents NO client cert.
	pool := x509.NewCertPool()
	caPEM, _ := os.ReadFile(p.caCrt)
	pool.AppendCertsFromPEM(caPEM)
	err := rawDial(url, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
	if err == nil {
		t.Fatal("dial without client cert succeeded, want mTLS rejection")
	}
}

func TestSidechannel_RejectsForeignClientCert(t *testing.T) {
	dir := t.TempDir()
	p := genCerts(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, url := startServer(t, ctx, p)

	// A client cert signed by a DIFFERENT CA than the server trusts.
	otherCAKey, otherCACert, _ := newCA(t)
	fKey, fDER := leaf(t, otherCAKey, otherCACert, "intruder", x509.ExtKeyUsageClientAuth, nil)
	fKeyDER, _ := x509.MarshalPKCS8PrivateKey(fKey)
	foreignCert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: fDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: fKeyDER}),
	)
	if err != nil {
		t.Fatalf("foreign keypair: %v", err)
	}
	pool := x509.NewCertPool()
	caPEM, _ := os.ReadFile(p.caCrt)
	pool.AppendCertsFromPEM(caPEM)

	err = rawDial(url, &tls.Config{
		Certificates: []tls.Certificate{foreignCert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	})
	if err == nil {
		t.Fatal("dial with foreign-signed client cert succeeded, want mTLS rejection")
	}
}

func TestSidechannel_ReconnectsAfterServerDrop(t *testing.T) {
	dir := t.TempDir()
	p := genCerts(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Server 1 on an ephemeral port we will reuse.
	ln1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln1.Addr().String()
	srv1, err := NewServer(ServerOptions{Listener: ln1, CACertPath: p.caCrt, ServerCert: p.serverCrt, ServerKey: p.serverKey, Log: quietLogger()})
	if err != nil {
		t.Fatalf("NewServer 1: %v", err)
	}
	ctx1, cancel1 := context.WithCancel(ctx)
	go func() { _ = srv1.Run(ctx1) }()

	pong := make(chan struct{}, 64)
	cli, err := NewClient(ClientOptions{
		URL:            "wss://" + addr + "/sidechannel",
		CACertPath:     p.caCrt,
		ClientCert:     p.clientCrt,
		ClientKey:      p.clientKey,
		Log:            quietLogger(),
		PingInterval:   50 * time.Millisecond,
		InitialBackoff: 50 * time.Millisecond,
		OnPong: func() {
			select {
			case pong <- struct{}{}:
			default:
			}
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	go func() { _ = cli.Run(ctx) }()

	// First connection works.
	select {
	case <-pong:
	case <-time.After(5 * time.Second):
		t.Fatal("no pong from server 1 within 5s")
	}
	waitFor(t, func() bool { return srv1.ConnCount() >= 1 }, 5*time.Second)

	// Drop server 1; its listener closes and the client connection
	// breaks, forcing the reconnect loop.
	cancel1()

	// Rebind the same port for server 2 (retry while the old listener
	// finishes closing).
	var ln2 net.Listener
	waitFor(t, func() bool {
		l, e := net.Listen("tcp", addr)
		if e != nil {
			return false
		}
		ln2 = l
		return true
	}, 5*time.Second)
	srv2, err := NewServer(ServerOptions{Listener: ln2, CACertPath: p.caCrt, ServerCert: p.serverCrt, ServerKey: p.serverKey, Log: quietLogger()})
	if err != nil {
		t.Fatalf("NewServer 2: %v", err)
	}
	go func() { _ = srv2.Run(ctx) }()

	// The client must redial and land on server 2.
	waitFor(t, func() bool { return srv2.ConnCount() >= 1 }, 8*time.Second)
}

func TestSidechannel_PublishControlRoundtrip(t *testing.T) {
	dir := t.TempDir()
	p := genCerts(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const streamID = "0c:ea:14:00:00:01"
	started := make(chan [2]string, 4)
	stopped := make(chan [2]string, 4)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv, err := NewServer(ServerOptions{
		Listener:       ln,
		CACertPath:     p.caCrt,
		ServerCert:     p.serverCrt,
		ServerKey:      p.serverKey,
		Log:            quietLogger(),
		OnStartPublish: func(id, tok string) { started <- [2]string{id, tok} },
		OnStopPublish:  func(id, reason string) { stopped <- [2]string{id, reason} },
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go func() { _ = srv.Run(ctx) }()

	url := "wss://" + ln.Addr().String() + "/sidechannel"
	var cli *Client
	cli, err = NewClient(ClientOptions{
		URL:            url,
		CACertPath:     p.caCrt,
		ClientCert:     p.clientCrt,
		ClientKey:      p.clientKey,
		Log:            quietLogger(),
		PingInterval:   50 * time.Millisecond,
		InitialBackoff: 50 * time.Millisecond,
		// Minimal edge stand-in: reply to request_publish with a
		// start_publish carrying a token. (The real authz+token logic
		// is the EdgePublisher, tested separately.)
		OnRequestPublish: func(id string, _ []streampublish.ICEServer) {
			cli.Send(Envelope{Type: TypeStartPublish, StreamID: id, PublishToken: "tok-" + id})
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	go func() { _ = cli.Run(ctx) }()

	waitFor(t, func() bool { return srv.ConnCount() >= 1 }, 5*time.Second)

	if sent := srv.RequestPublish(ctx, streamID); sent < 1 {
		t.Fatalf("RequestPublish reached %d edges, want >= 1", sent)
	}

	select {
	case got := <-started:
		if got[0] != streamID || got[1] != "tok-"+streamID {
			t.Errorf("start_publish = %v, want [%s tok-%s]", got, streamID, streamID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no start_publish within 5s")
	}
	waitFor(t, func() bool {
		for _, id := range srv.ActiveStreams() {
			if id == streamID {
				return true
			}
		}
		return false
	}, 3*time.Second)

	cli.Send(Envelope{Type: TypeStopPublish, StreamID: streamID, Reason: ReasonNoSubscribers})
	select {
	case got := <-stopped:
		if got[0] != streamID || got[1] != ReasonNoSubscribers {
			t.Errorf("stop_publish = %v, want [%s %s]", got, streamID, ReasonNoSubscribers)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no stop_publish within 5s")
	}
	waitFor(t, func() bool { return len(srv.ActiveStreams()) == 0 }, 3*time.Second)
}

// TestSidechannel_RequestStopRoundtrip proves the S20 cloud -> edge request_stop
// frame reaches the edge's OnRequestStop callback with the stream id intact -
// the wire half of "stop the publish bridge when the last cloud subscriber
// leaves". An empty stream id is a no-op (never broadcast).
func TestSidechannel_RequestStopRoundtrip(t *testing.T) {
	dir := t.TempDir()
	p := genCerts(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const streamID = "0c:ea:14:00:00:01"
	stops := make(chan string, 4)

	srv, url := startServer(t, ctx, p)

	cli, err := NewClient(ClientOptions{
		URL:            url,
		CACertPath:     p.caCrt,
		ClientCert:     p.clientCrt,
		ClientKey:      p.clientKey,
		Log:            quietLogger(),
		PingInterval:   50 * time.Millisecond,
		InitialBackoff: 50 * time.Millisecond,
		OnRequestStop:  func(id string) { stops <- id },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	go func() { _ = cli.Run(ctx) }()

	waitFor(t, func() bool { return srv.ConnCount() >= 1 }, 5*time.Second)

	if sent := srv.RequestStop(ctx, streamID); sent < 1 {
		t.Fatalf("RequestStop reached %d edges, want >= 1", sent)
	}
	select {
	case got := <-stops:
		if got != streamID {
			t.Errorf("request_stop stream_id = %q, want %q", got, streamID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no request_stop within 5s")
	}

	// An empty stream id is rejected before any broadcast.
	if sent := srv.RequestStop(ctx, ""); sent != 0 {
		t.Errorf("RequestStop(\"\") reached %d edges, want 0", sent)
	}
}

// TestSidechannel_TURNTelemetryRoundtrip proves the cloud -> edge
// turn_event / turn_stats frames carry their payloads (incl. the
// nullable AuthOK and the config view) intact across the link.
func TestSidechannel_TURNTelemetryRoundtrip(t *testing.T) {
	dir := t.TempDir()
	p := genCerts(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, url := startServer(t, ctx, p)

	events := make(chan turnstore.Event, 8)
	snaps := make(chan turnstore.Snapshot, 8)
	cli, err := NewClient(ClientOptions{
		URL:            url,
		CACertPath:     p.caCrt,
		ClientCert:     p.clientCrt,
		ClientKey:      p.clientKey,
		Log:            quietLogger(),
		PingInterval:   50 * time.Millisecond,
		InitialBackoff: 50 * time.Millisecond,
		OnTURNEvent:    func(e turnstore.Event) { events <- e },
		OnTURNStats:    func(s turnstore.Snapshot) { snaps <- s },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	go func() { _ = cli.Run(ctx) }()

	waitFor(t, func() bool { return srv.ConnCount() >= 1 }, 5*time.Second)

	yes := true
	if sent := srv.SendTURNEvent(ctx, turnstore.Event{
		Kind: "auth", SrcMasked: "v4:203.0.x.x#a1", Username: "carvilon", Realm: "carvilon", AuthOK: &yes,
	}); sent < 1 {
		t.Fatalf("SendTURNEvent reached %d edges, want >= 1", sent)
	}
	select {
	case e := <-events:
		if e.Kind != "auth" || e.SrcMasked != "v4:203.0.x.x#a1" || e.AuthOK == nil || !*e.AuthOK {
			t.Errorf("turn_event = %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no turn_event within 5s")
	}

	if sent := srv.SendTURNStats(ctx, turnstore.Snapshot{
		Enabled: true, AllocationCount: 3, UDPPort: 3478, TLSPort: 5349, Realm: "carvilon", STUNActive: true,
	}); sent < 1 {
		t.Fatalf("SendTURNStats reached %d edges, want >= 1", sent)
	}
	select {
	case s := <-snaps:
		if !s.Enabled || s.AllocationCount != 3 || s.UDPPort != 3478 || s.TLSPort != 5349 || !s.STUNActive {
			t.Errorf("turn_stats = %+v", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no turn_stats within 5s")
	}
}

// TestSidechannel_StreamStatsRoundtrip proves the cloud -> edge
// stream_stats frame carries the per-stream WHEP consumer counts intact
// across the link (S20 step 2 fix harness): a snapshot sent with
// Consumers=1 must arrive at the edge callback as 1, not 0, with the
// StreamID (viewer MAC) preserved. The egress mirror of the TURN
// telemetry roundtrip above.
func TestSidechannel_StreamStatsRoundtrip(t *testing.T) {
	dir := t.TempDir()
	p := genCerts(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, url := startServer(t, ctx, p)

	snaps := make(chan streamstore.Snapshot, 8)
	cli, err := NewClient(ClientOptions{
		URL:            url,
		CACertPath:     p.caCrt,
		ClientCert:     p.clientCrt,
		ClientKey:      p.clientKey,
		Log:            quietLogger(),
		PingInterval:   50 * time.Millisecond,
		InitialBackoff: 50 * time.Millisecond,
		OnStreamStats:  func(s streamstore.Snapshot) { snaps <- s },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	go func() { _ = cli.Run(ctx) }()

	waitFor(t, func() bool { return srv.ConnCount() >= 1 }, 5*time.Second)

	sentAt := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	if sent := srv.SendStreamStats(ctx, streamstore.Snapshot{
		GeneratedAt: sentAt,
		Streams:     []streamstore.Stat{{StreamID: "0c:ea:14:00:00:01", Consumers: 1}},
	}); sent < 1 {
		t.Fatalf("SendStreamStats reached %d edges, want >= 1", sent)
	}
	select {
	case s := <-snaps:
		if len(s.Streams) != 1 {
			t.Fatalf("stream_stats arrived with %d streams, want 1: %+v", len(s.Streams), s)
		}
		if s.Streams[0].StreamID != "0c:ea:14:00:00:01" || s.Streams[0].Consumers != 1 {
			t.Errorf("stream_stats = %+v, want StreamID=0c:ea:14:00:00:01 Consumers=1", s.Streams[0])
		}
		if !s.GeneratedAt.Equal(sentAt) {
			t.Errorf("GeneratedAt = %v, want %v", s.GeneratedAt, sentAt)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no stream_stats within 5s")
	}

	// An EMPTY snapshot ("cloud up, 0 viewers") must also arrive - the edge
	// distinguishes it from "cloud down" by freshness alone.
	if sent := srv.SendStreamStats(ctx, streamstore.Snapshot{GeneratedAt: sentAt}); sent < 1 {
		t.Fatalf("empty SendStreamStats reached %d edges, want >= 1", sent)
	}
	select {
	case s := <-snaps:
		if len(s.Streams) != 0 {
			t.Errorf("empty stream_stats arrived with %d streams, want 0", len(s.Streams))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no empty stream_stats within 5s")
	}
}

// rawDial attempts a WebSocket dial with an explicit TLS config and
// returns the dial error (nil on success). Used to assert mTLS
// rejections from a client that does not go through sidechannel.Client.
func rawDial(url string, tlsCfg *tls.Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: hc})
	if err == nil {
		conn.CloseNow()
	}
	return err
}
