package state

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testMockID = "0cea14424242"

func fakeBundle() *Bundle {
	return &Bundle{
		BrokerAddress:  "tls://192.168.1.1:12812",
		BrokerCert:     "-----BEGIN CERTIFICATE-----\nfakecert\n-----END CERTIFICATE-----",
		BrokerCertCA:   "-----BEGIN CERTIFICATE-----\nfakeca\n-----END CERTIFICATE-----",
		BrokerPrivKey:  "-----BEGIN EC PRIVATE KEY-----\nfakekey\n-----END EC PRIVATE KEY-----",
		ControllerID:   "0cea14122cfd",
		ControllerType: "ULP-Go",
		Extras: Extras{
			DoorID:              "11111111-2222-3333-4444-555555555555",
			DoorName:            "UDM SE",
			HTTPCertFingerprint: "abc",
			MQTTCertFingerprint: "def",
			NACLPrivKey:         "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
			NACLPubKey:          "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=",
		},
		Name:        "UA Intercom Viewer 4242",
		SSHPassword: "test-password",
		SSHUser:     "ubnt",
	}
}

func TestNew_CreatesBaseDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "state")
	if _, err := New(dir); err != nil {
		t.Fatalf("New: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("base dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("base path is not a directory")
	}
}

func TestSaveAndLoadBundle_Roundtrip(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	in := fakeBundle()
	if err := store.SaveBundle(testMockID, in); err != nil {
		t.Fatalf("SaveBundle: %v", err)
	}
	out, err := store.LoadBundle(testMockID)
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}
	if out == nil {
		t.Fatal("LoadBundle returned nil after SaveBundle")
	}
	if out.BrokerAddress != in.BrokerAddress {
		t.Errorf("BrokerAddress = %q, want %q", out.BrokerAddress, in.BrokerAddress)
	}
	if out.Extras.DoorID != in.Extras.DoorID {
		t.Errorf("Extras.DoorID = %q, want %q", out.Extras.DoorID, in.Extras.DoorID)
	}
	if out.SSHUser != in.SSHUser {
		t.Errorf("SSHUser = %q, want %q", out.SSHUser, in.SSHUser)
	}
}

func TestLoadBundle_NotPresent(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b, err := store.LoadBundle("not-adopted")
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}
	if b != nil {
		t.Errorf("LoadBundle returned %+v, want nil", b)
	}
}

func TestLoadMeta_NotPresent(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, err := store.LoadMeta("not-adopted")
	if err != nil {
		t.Fatalf("LoadMeta: %v", err)
	}
	if m != nil {
		t.Errorf("LoadMeta returned %+v, want nil", m)
	}
}

func TestSaveBundle_MetaCompleteFlag(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := store.SaveBundle(testMockID, fakeBundle()); err != nil {
		t.Fatalf("SaveBundle: %v", err)
	}
	complete, err := store.BundleComplete(testMockID)
	if err != nil {
		t.Fatalf("BundleComplete: %v", err)
	}
	if !complete {
		t.Error("BundleComplete = false, want true after SaveBundle")
	}
}

func TestBundleComplete_BeforeSave(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	complete, err := store.BundleComplete(testMockID)
	if err != nil {
		t.Fatalf("BundleComplete: %v", err)
	}
	if complete {
		t.Error("BundleComplete = true before SaveBundle")
	}
}

func TestSaveBundle_CertExtraction(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	in := fakeBundle()
	if err := store.SaveBundle(testMockID, in); err != nil {
		t.Fatalf("SaveBundle: %v", err)
	}
	checks := []struct {
		name string
		want string
	}{
		{"broker.crt", in.BrokerCert},
		{"broker.key", in.BrokerPrivKey},
		{"broker_ca.crt", in.BrokerCertCA},
	}
	for _, c := range checks {
		path := filepath.Join(store.CertDir(testMockID), c.name)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read %s: %v", c.name, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("%s content mismatch", c.name)
		}
	}
}

func TestSaveBundle_NoLeftoverTempFiles(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := store.SaveBundle(testMockID, fakeBundle()); err != nil {
		t.Fatalf("SaveBundle: %v", err)
	}
	entries, err := os.ReadDir(store.MockDir(testMockID))
	if err != nil {
		t.Fatalf("read mock dir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp.") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestSaveBundle_FilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions not enforced on windows")
	}
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := store.SaveBundle(testMockID, fakeBundle()); err != nil {
		t.Fatalf("SaveBundle: %v", err)
	}
	files := []string{
		filepath.Join(store.MockDir(testMockID), "bundle.json"),
		filepath.Join(store.MockDir(testMockID), "meta.json"),
		filepath.Join(store.CertDir(testMockID), "broker.crt"),
		filepath.Join(store.CertDir(testMockID), "broker.key"),
		filepath.Join(store.CertDir(testMockID), "broker_ca.crt"),
	}
	for _, p := range files {
		info, err := os.Stat(p)
		if err != nil {
			t.Errorf("stat %s: %v", p, err)
			continue
		}
		mode := info.Mode().Perm()
		if mode != 0o600 {
			t.Errorf("%s mode = %o, want 0600", p, mode)
		}
	}
}

func TestSaveBundle_Overwrite(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	first := fakeBundle()
	if err := store.SaveBundle(testMockID, first); err != nil {
		t.Fatalf("first SaveBundle: %v", err)
	}
	second := fakeBundle()
	second.SSHPassword = "different-password"
	if err := store.SaveBundle(testMockID, second); err != nil {
		t.Fatalf("second SaveBundle: %v", err)
	}
	loaded, err := store.LoadBundle(testMockID)
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}
	if loaded.SSHPassword != "different-password" {
		t.Errorf("SSHPassword = %q, want %q (overwrite failed)",
			loaded.SSHPassword, "different-password")
	}
}
