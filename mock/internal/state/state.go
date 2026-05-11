// Package state persists per-mock adoption state on disk.
//
// Layout:
//
//	state/
//	  <mock-id>/
//	    bundle.json   the full UDM adoption payload
//	    meta.json     adopted-timestamp, complete-flag
//	    certs/
//	      broker.crt      from bundle.broker_cert
//	      broker.key      from bundle.broker_priv_key
//	      broker_ca.crt   from bundle.broker_cert_ca
//	      server.crt      self-signed cert used for the HTTPS adoption server
//	      server.key      private key paired with server.crt
//
// All files are created with mode 0600, all directories 0700.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	dirMode  os.FileMode = 0o700
	fileMode os.FileMode = 0o600
)

// Bundle is the full UDM adoption payload, saison 9 pcap-verified
// field schema. Names match the wire format exactly. Extras is
// nested because UDM puts door_id and the nacl keys there.
type Bundle struct {
	BrokerAddress  string `json:"broker_address"`
	BrokerCert     string `json:"broker_cert"`
	BrokerCertCA   string `json:"broker_cert_ca"`
	BrokerPrivKey  string `json:"broker_priv_key"`
	ControllerID   string `json:"controller_id"`
	ControllerType string `json:"controller_type"`
	Extras         Extras `json:"extras"`
	Name           string `json:"name"`
	SSHPassword    string `json:"ssh_password"`
	SSHUser        string `json:"ssh_user"`
}

// Extras carries fields that UDM nests under "extras".
type Extras struct {
	DoorID              string `json:"door_id"`
	DoorName            string `json:"door_name"`
	HTTPCertFingerprint string `json:"http_cert_fingerprint"`
	MQTTCertFingerprint string `json:"mqtt_cert_fingerprint"`
	NACLPrivKey         string `json:"nacl_priv_key"`
	NACLPubKey          string `json:"nacl_pub_key"`
}

// Meta tracks runtime state per mock.
type Meta struct {
	MockID         string    `json:"mock_id"`
	AdoptedAt      time.Time `json:"adopted_at"`
	BundleComplete bool      `json:"bundle_complete"`
}

// Store persists per-mock state under a base directory.
type Store struct {
	baseDir string
	mu      sync.Mutex
}

// New constructs a Store rooted at baseDir, creating baseDir if
// missing.
func New(baseDir string) (*Store, error) {
	if baseDir == "" {
		return nil, errors.New("state: baseDir must not be empty")
	}
	if err := os.MkdirAll(baseDir, dirMode); err != nil {
		return nil, fmt.Errorf("state: mkdir %s: %w", baseDir, err)
	}
	return &Store{baseDir: baseDir}, nil
}

// BaseDir returns the root directory of this Store.
func (s *Store) BaseDir() string { return s.baseDir }

// MockDir returns the per-mock directory path.
func (s *Store) MockDir(mockID string) string {
	return filepath.Join(s.baseDir, mockID)
}

// CertDir returns the per-mock cert directory path.
func (s *Store) CertDir(mockID string) string {
	return filepath.Join(s.baseDir, mockID, "certs")
}

// SaveBundle writes the bundle JSON, extracts the broker PEM
// material into separate files, and writes a meta.json marking
// the bundle complete.
func (s *Store) SaveBundle(mockID string, b *Bundle) error {
	if mockID == "" {
		return errors.New("state: mockID must not be empty")
	}
	if b == nil {
		return errors.New("state: bundle must not be nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	mockDir := s.MockDir(mockID)
	certDir := s.CertDir(mockID)
	if err := os.MkdirAll(certDir, dirMode); err != nil {
		return fmt.Errorf("state: mkdir certs: %w", err)
	}
	if err := os.Chmod(mockDir, dirMode); err != nil {
		return fmt.Errorf("state: chmod mock dir: %w", err)
	}

	bundleBytes, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("state: marshal bundle: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(mockDir, "bundle.json"), bundleBytes); err != nil {
		return err
	}

	meta := &Meta{
		MockID:         mockID,
		AdoptedAt:      time.Now().UTC(),
		BundleComplete: true,
	}
	metaBytes, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("state: marshal meta: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(mockDir, "meta.json"), metaBytes); err != nil {
		return err
	}

	certFiles := []struct {
		name    string
		content string
	}{
		{"broker.crt", b.BrokerCert},
		{"broker.key", b.BrokerPrivKey},
		{"broker_ca.crt", b.BrokerCertCA},
	}
	for _, f := range certFiles {
		if err := writeFileAtomic(filepath.Join(certDir, f.name), []byte(f.content)); err != nil {
			return err
		}
	}
	return nil
}

// LoadBundle returns the persisted bundle, or (nil, nil) if the
// bundle has not been saved for this mock yet.
func (s *Store) LoadBundle(mockID string) (*Bundle, error) {
	path := filepath.Join(s.MockDir(mockID), "bundle.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("state: read bundle: %w", err)
	}
	var b Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("state: unmarshal bundle: %w", err)
	}
	return &b, nil
}

// LoadMeta returns the persisted meta, or (nil, nil) if meta.json
// has not been written for this mock yet.
func (s *Store) LoadMeta(mockID string) (*Meta, error) {
	path := filepath.Join(s.MockDir(mockID), "meta.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("state: read meta: %w", err)
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("state: unmarshal meta: %w", err)
	}
	return &m, nil
}

// BundleComplete reports whether bundle persistence has finished
// for this mock.
func (s *Store) BundleComplete(mockID string) (bool, error) {
	m, err := s.LoadMeta(mockID)
	if err != nil {
		return false, err
	}
	if m == nil {
		return false, nil
	}
	return m.BundleComplete, nil
}

// writeFileAtomic writes data to a temp file in the same directory
// and renames it into place. The rename is atomic on POSIX, so a
// crash mid-write cannot leave a partial target file. Sets 0600
// permissions on the final file.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("state: create temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("state: write temp: %w", err)
	}
	if err := tmp.Chmod(fileMode); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("state: chmod temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("state: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("state: rename %s -> %s: %w", tmpName, path, err)
	}
	return nil
}
