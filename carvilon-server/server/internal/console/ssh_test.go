package console

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// ---- in-process test SSH server ----

func newTestSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen host key: %v", err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return s
}

// startTestSSHServer runs a minimal SSH server that accepts the given
// password and/or authorized key and echoes stdin back on the session's
// shell. Returns its listen address; the listener closes on cleanup.
func startTestSSHServer(t *testing.T, hostKey ssh.Signer, password string, authorized ssh.PublicKey) string {
	t.Helper()
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if password != "" && string(pass) == password {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("password rejected")
		},
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if authorized != nil && bytes.Equal(key.Marshal(), authorized.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("key rejected")
		},
	}
	cfg.AddHostKey(hostKey)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go serveTestSSHConn(nc, cfg)
		}
	}()
	return ln.Addr().String()
}

func serveTestSSHConn(nc net.Conn, cfg *ssh.ServerConfig) {
	sconn, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		_ = nc.Close()
		return
	}
	defer sconn.Close()
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "only session")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			return
		}
		go func() {
			for req := range chReqs {
				switch req.Type {
				case "pty-req", "shell", "window-change":
					_ = req.Reply(true, nil)
				default:
					_ = req.Reply(false, nil)
				}
			}
		}()
		// Echo stdin back to stdout until the client hangs up.
		go func() {
			_, _ = io.Copy(ch, ch)
			_ = ch.Close()
		}()
	}
}

// memHostKeys is an in-memory TOFU store for tests.
type memHostKeys struct {
	mu sync.Mutex
	m  map[string]string
}

func newMemHostKeys() *memHostKeys { return &memHostKeys{m: map[string]string{}} }

func (h *memHostKeys) Lookup(hostPort string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.m[hostPort], nil
}

func (h *memHostKeys) Pin(hostPort, _ /*keyType*/, fingerprint string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.m[hostPort] = fingerprint
	return nil
}

func (h *memHostKeys) get(hostPort string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.m[hostPort]
}

func splitAddr(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	port, _ := strconv.Atoi(portStr)
	return host, port
}

// readWithin reads once from the backend, failing if nothing arrives.
func readWithin(t *testing.T, b Backend, d time.Duration) []byte {
	t.Helper()
	type res struct {
		p   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() {
		buf := make([]byte, 256)
		n, err := b.Read(buf)
		ch <- res{buf[:n], err}
	}()
	select {
	case r := <-ch:
		return r.p
	case <-time.After(d):
		t.Fatal("backend read timed out")
		return nil
	}
}

// clientKeyPEM generates an ed25519 key and returns its OpenSSH PEM plus
// the public key for the server's authorized set.
func clientKeyPEM(t *testing.T, passphrase string) ([]byte, ssh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen client key: %v", err)
	}
	var block *pem.Block
	if passphrase != "" {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "test", []byte(passphrase))
	} else {
		block, err = ssh.MarshalPrivateKey(priv, "test")
	}
	if err != nil {
		t.Fatalf("marshal client key: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("public key: %v", err)
	}
	return pem.EncodeToMemory(block), sshPub
}

// ---- tests ----

func TestDialSSH_PasswordAuthAndTOFUPin(t *testing.T) {
	hostKey := newTestSigner(t)
	addr := startTestSSHServer(t, hostKey, "goodpass", nil)
	host, port := splitAddr(t, addr)
	hk := newMemHostKeys()

	b, err := DialSSH(SSHConfig{Host: host, Port: port, User: "u", Password: "goodpass", HostKeys: hk})
	if err != nil {
		t.Fatalf("DialSSH: %v", err)
	}
	defer b.Close()

	if _, err := b.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := readWithin(t, b, 2*time.Second); !bytes.Contains(got, []byte("hello")) {
		t.Fatalf("echo = %q, want it to contain hello", got)
	}
	// TOFU pinned the server key on first use.
	want := ssh.FingerprintSHA256(hostKey.PublicKey())
	if got := hk.get(addr); got != want {
		t.Fatalf("pinned fingerprint = %q, want %q", got, want)
	}
}

func TestDialSSH_KeyAuth(t *testing.T) {
	hostKey := newTestSigner(t)
	keyPEM, pub := clientKeyPEM(t, "")
	addr := startTestSSHServer(t, hostKey, "", pub)
	host, port := splitAddr(t, addr)

	b, err := DialSSH(SSHConfig{Host: host, Port: port, User: "u", PrivateKey: keyPEM, HostKeys: newMemHostKeys()})
	if err != nil {
		t.Fatalf("DialSSH key: %v", err)
	}
	defer b.Close()
	if _, err := b.Write([]byte("ping\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := readWithin(t, b, 2*time.Second); !bytes.Contains(got, []byte("ping")) {
		t.Fatalf("echo = %q, want ping", got)
	}
}

func TestDialSSH_EncryptedKeyWithPassphrase(t *testing.T) {
	hostKey := newTestSigner(t)
	keyPEM, pub := clientKeyPEM(t, "s3kritphrase")
	addr := startTestSSHServer(t, hostKey, "", pub)
	host, port := splitAddr(t, addr)

	b, err := DialSSH(SSHConfig{
		Host: host, Port: port, User: "u",
		PrivateKey: keyPEM, Passphrase: "s3kritphrase", HostKeys: newMemHostKeys(),
	})
	if err != nil {
		t.Fatalf("DialSSH encrypted key: %v", err)
	}
	_ = b.Close()
}

func TestDialSSH_WrongPasswordRejected(t *testing.T) {
	hostKey := newTestSigner(t)
	addr := startTestSSHServer(t, hostKey, "goodpass", nil)
	host, port := splitAddr(t, addr)

	b, err := DialSSH(SSHConfig{Host: host, Port: port, User: "u", Password: "WRONG", HostKeys: newMemHostKeys()})
	if err == nil {
		_ = b.Close()
		t.Fatal("DialSSH accepted a wrong password")
	}
}

func TestDialSSH_CredentialNeverInError(t *testing.T) {
	hostKey := newTestSigner(t)
	addr := startTestSSHServer(t, hostKey, "goodpass", nil)
	host, port := splitAddr(t, addr)

	const secret = "S3cr3t-must-not-leak-42"
	_, err := DialSSH(SSHConfig{Host: host, Port: port, User: "u", Password: secret, HostKeys: newMemHostKeys()})
	if err == nil {
		t.Fatal("expected auth failure")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("password leaked into error: %v", err)
	}
}

func TestDialSSH_ChangedHostKeyBlockedThenRetrust(t *testing.T) {
	hostKey := newTestSigner(t)
	addr := startTestSSHServer(t, hostKey, "goodpass", nil)
	host, port := splitAddr(t, addr)

	// A stale/wrong pin simulates the server key having changed.
	hk := newMemHostKeys()
	_ = hk.Pin(addr, "ssh-ed25519", "SHA256:staleAAAstaleBBBstaleCCC")

	_, err := DialSSH(SSHConfig{Host: host, Port: port, User: "u", Password: "goodpass", HostKeys: hk})
	var changed *HostKeyChangedError
	if !errors.As(err, &changed) {
		t.Fatalf("expected HostKeyChangedError, got %v", err)
	}
	if changed.HostPort != addr {
		t.Fatalf("changed.HostPort = %q, want %q", changed.HostPort, addr)
	}

	// Explicit re-trust: forget the stale pin, reconnect, new key pins.
	hk.mu.Lock()
	delete(hk.m, addr)
	hk.mu.Unlock()

	b, err := DialSSH(SSHConfig{Host: host, Port: port, User: "u", Password: "goodpass", HostKeys: hk})
	if err != nil {
		t.Fatalf("re-trust dial: %v", err)
	}
	_ = b.Close()
	if got, want := hk.get(addr), ssh.FingerprintSHA256(hostKey.PublicKey()); got != want {
		t.Fatalf("re-pinned fingerprint = %q, want %q", got, want)
	}
}
