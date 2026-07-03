// Outbound SSH client backend. Cross-platform (works on the dev host
// too): dial host:port, authenticate by password OR private key (with an
// optional passphrase), then run a shell on a remote PTY. Host keys are
// verified trust-on-first-use: the first fingerprint is pinned; a later
// change blocks the connection until an operator explicitly re-trusts.
package console

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// defaultDialTimeout bounds the TCP+handshake phase.
const defaultDialTimeout = 15 * time.Second

// HostKeyStore pins and verifies server host keys (TOFU). It is backed
// by consolestore in production; tests use an in-memory fake.
type HostKeyStore interface {
	// Lookup returns the pinned fingerprint for hostPort, or "" if none
	// is pinned yet (first use).
	Lookup(hostPort string) (fingerprint string, err error)
	// Pin records fingerprint as trusted for hostPort (first use, or an
	// explicit re-trust replacing a previous pin).
	Pin(hostPort, keyType, fingerprint string) error
}

// HostKeyChangedError is returned when the presented host key does not
// match the pinned one — a possible man-in-the-middle. The connection is
// refused; the operator must explicitly re-trust before it can proceed.
type HostKeyChangedError struct {
	HostPort  string
	Pinned    string
	Presented string
	KeyType   string
}

func (e *HostKeyChangedError) Error() string {
	return fmt.Sprintf("console: host key for %s changed (pinned %s, presented %s) — connection refused",
		e.HostPort, e.Pinned, e.Presented)
}

// SSHConfig describes one outbound SSH connection. Exactly one of
// Password / PrivateKey must be set.
type SSHConfig struct {
	Host       string
	Port       int
	User       string
	Password   string
	PrivateKey []byte // PEM (OpenSSH or PKCS#1/8)
	Passphrase string // optional, for an encrypted PrivateKey
	HostKeys   HostKeyStore
	Timeout    time.Duration
	// Cols/Rows seed the remote PTY window; zero falls back to 80x24.
	Cols uint16
	Rows uint16
}

// tofuHostKeyCallback verifies the server host key against the pinned
// fingerprint, pinning on first use and refusing a changed key.
func tofuHostKeyCallback(store HostKeyStore, hostPort string) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		fp := ssh.FingerprintSHA256(key)
		pinned, err := store.Lookup(hostPort)
		if err != nil {
			return fmt.Errorf("console: host-key lookup: %w", err)
		}
		if pinned == "" {
			return store.Pin(hostPort, key.Type(), fp) // trust on first use
		}
		if pinned != fp {
			return &HostKeyChangedError{HostPort: hostPort, Pinned: pinned, Presented: fp, KeyType: key.Type()}
		}
		return nil
	}
}

// DialSSH connects, authenticates, requests a PTY, and starts a shell,
// returning the live session as a Backend. Credential material is never
// included in returned errors or logged.
func DialSSH(cfg SSHConfig) (Backend, error) {
	if cfg.HostKeys == nil {
		return nil, errors.New("console: ssh requires a host-key store")
	}
	port := cfg.Port
	if port <= 0 || port > 65535 {
		port = 22
	}
	auth, err := authMethods(cfg)
	if err != nil {
		return nil, err
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultDialTimeout
	}
	hostPort := net.JoinHostPort(cfg.Host, strconv.Itoa(port))
	clientCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            auth,
		HostKeyCallback: tofuHostKeyCallback(cfg.HostKeys, hostPort),
		Timeout:         timeout,
	}

	client, err := ssh.Dial("tcp", hostPort, clientCfg)
	if err != nil {
		// ssh.Dial errors never carry the password/key, but they may wrap
		// our HostKeyChangedError — surface that unchanged for the caller.
		var changed *HostKeyChangedError
		if errors.As(err, &changed) {
			return nil, changed
		}
		return nil, fmt.Errorf("console: ssh dial %s: %w", hostPort, err)
	}

	sess, err := client.NewSession()
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("console: ssh session: %w", err)
	}

	pr, pw := io.Pipe()
	sess.Stdout = pw
	sess.Stderr = pw
	stdin, err := sess.StdinPipe()
	if err != nil {
		_ = sess.Close()
		_ = client.Close()
		return nil, fmt.Errorf("console: ssh stdin: %w", err)
	}

	cols, rows := cfg.Cols, cfg.Rows
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}
	if err := sess.RequestPty("xterm-256color", int(rows), int(cols), modes); err != nil {
		_ = sess.Close()
		_ = client.Close()
		return nil, fmt.Errorf("console: request pty: %w", err)
	}
	if err := sess.Shell(); err != nil {
		_ = sess.Close()
		_ = client.Close()
		return nil, fmt.Errorf("console: start shell: %w", err)
	}

	b := &sshBackend{client: client, sess: sess, stdin: stdin, pr: pr, pw: pw}
	// When the remote shell exits, unblock Read with EOF.
	go func() { _ = sess.Wait(); _ = pw.Close() }()
	return b, nil
}

// authMethods builds the ssh auth list from exactly one credential.
func authMethods(cfg SSHConfig) ([]ssh.AuthMethod, error) {
	switch {
	case len(cfg.PrivateKey) > 0:
		var signer ssh.Signer
		var err error
		if cfg.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(cfg.PrivateKey, []byte(cfg.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey(cfg.PrivateKey)
		}
		if err != nil {
			// ssh.ParsePrivateKey errors describe the format, never the key.
			return nil, fmt.Errorf("console: parse private key: %w", err)
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	case cfg.Password != "":
		return []ssh.AuthMethod{ssh.Password(cfg.Password)}, nil
	default:
		return nil, errors.New("console: no ssh credential provided")
	}
}

// sshBackend bridges the console frame to a remote SSH shell. Backend
// output (stdout+stderr) is merged onto one pipe read by the frame.
type sshBackend struct {
	client    *ssh.Client
	sess      *ssh.Session
	stdin     io.WriteCloser
	pr        *io.PipeReader
	pw        *io.PipeWriter
	closeOnce sync.Once
}

func (b *sshBackend) Read(p []byte) (int, error)  { return b.pr.Read(p) }
func (b *sshBackend) Write(p []byte) (int, error) { return b.stdin.Write(p) }

func (b *sshBackend) Resize(cols, rows uint16) error {
	return b.sess.WindowChange(int(rows), int(cols))
}

// Close tears the SSH connection down fully. Idempotent.
func (b *sshBackend) Close() error {
	b.closeOnce.Do(func() {
		_ = b.pw.Close() // unblock Read
		_ = b.stdin.Close()
		_ = b.sess.Close()
		_ = b.client.Close()
	})
	return nil
}
