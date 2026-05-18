package mockmanager

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"carvilon.local/mock"
	"carvilon.local/server/internal/db"
)

// fakeViewer satisfies the Viewer interface without touching the
// network. Tests build a small graph of these to exercise the
// manager lifecycle.
type fakeViewer struct {
	mac      string
	events   chan mock.DoorbellEvent
	cancels  chan mock.DoorbellCancelEvent
	runs     atomic.Int32
	stopped  chan struct{}
	stopOnce sync.Once
}

func newFakeViewer(mac string) *fakeViewer {
	return &fakeViewer{
		mac:     mac,
		events:  make(chan mock.DoorbellEvent, 4),
		cancels: make(chan mock.DoorbellCancelEvent, 4),
		stopped: make(chan struct{}),
	}
}

func (f *fakeViewer) Run(ctx context.Context) error {
	f.runs.Add(1)
	<-ctx.Done()
	f.stopOnce.Do(func() { close(f.stopped) })
	return ctx.Err()
}

func (f *fakeViewer) Events() <-chan mock.DoorbellEvent        { return f.events }
func (f *fakeViewer) Cancels() <-chan mock.DoorbellCancelEvent { return f.cancels }
func (f *fakeViewer) MAC() string                              { return f.mac }
func (f *fakeViewer) RejectDoorbell(intercomMAC string) error  { return nil }

// fakeFactory records every viewer it creates so tests can
// assert on goroutine start counts and reach into the fakes.
type fakeFactory struct {
	mu      sync.Mutex
	viewers map[string]*fakeViewer
	starts  atomic.Int32
}

func newFakeFactory() *fakeFactory {
	return &fakeFactory{viewers: make(map[string]*fakeViewer)}
}

func (f *fakeFactory) make(cfg mock.Config, _ *slog.Logger) (Viewer, error) {
	f.starts.Add(1)
	v := newFakeViewer(cfg.MAC)
	f.mu.Lock()
	f.viewers[cfg.MAC] = v
	f.mu.Unlock()
	return v, nil
}

func (f *fakeFactory) viewer(mac string) *fakeViewer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.viewers[mac]
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestManager(t *testing.T) (*Manager, *fakeFactory) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	factory := newFakeFactory()
	mgr := New(d, quietLogger(), Options{
		StateDirBase: filepath.Join(t.TempDir(), "state"),
		ServerIPv4:   "127.0.0.1",
		Factory:      factory.make,
	})
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = mgr.Shutdown(shutCtx)
		_ = d.Close()
	})
	return mgr, factory
}

func sampleSpec(mac string, port uint16) ViewerSpec {
	return ViewerSpec{
		MAC:         mac,
		Name:        "viewer-" + mac,
		ServicePort: port,
	}
}

// ---------- LoadFromDB ----------

func TestLoadFromDB_Empty(t *testing.T) {
	mgr, factory := newTestManager(t)
	if err := mgr.LoadFromDB(context.Background()); err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}
	if got := factory.starts.Load(); got != 0 {
		t.Errorf("factory invocations = %d, want 0", got)
	}
}

func TestLoadFromDB_StartsPersistedViewers(t *testing.T) {
	mgr, factory := newTestManager(t)
	now := mgr.opts.Now().UnixMilli()
	specs := []ViewerSpec{
		sampleSpec("0c:ea:14:42:42:42", 8080),
		sampleSpec("0c:ea:14:42:42:43", 8081),
	}
	for _, s := range specs {
		_, err := mgr.db.Exec(
			`INSERT INTO viewers (mac, name, service_port, type, created_at, updated_at)
			 VALUES (?, ?, ?, 'web', ?, ?)`,
			s.MAC, s.Name, int64(s.ServicePort), now, now,
		)
		if err != nil {
			t.Fatalf("seed insert: %v", err)
		}
	}
	if err := mgr.LoadFromDB(context.Background()); err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}
	if got := factory.starts.Load(); got != 2 {
		t.Errorf("factory invocations = %d, want 2", got)
	}
	info, _ := mgr.ListViewers(context.Background())
	if len(info) != 2 {
		t.Errorf("ListViewers len = %d, want 2", len(info))
	}
}

// ---------- AddViewer ----------

func TestAddViewer_PersistsToDB(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	var name string
	var port int64
	err := mgr.db.QueryRow(
		`SELECT name, service_port FROM viewers WHERE mac = ?`, spec.MAC,
	).Scan(&name, &port)
	if err != nil {
		t.Fatalf("query viewers: %v", err)
	}
	if name != spec.Name {
		t.Errorf("persisted name = %q, want %q", name, spec.Name)
	}
	if uint16(port) != spec.ServicePort {
		t.Errorf("persisted port = %d, want %d", port, spec.ServicePort)
	}
}

func TestAddViewer_StartsGoroutine(t *testing.T) {
	mgr, factory := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	v := factory.viewer(spec.MAC)
	if v == nil {
		t.Fatal("factory did not record a viewer")
	}
	for i := 0; i < 100; i++ {
		if v.runs.Load() == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("Run was not invoked within 500ms")
}

func TestAddViewer_DuplicateMACError(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("first AddViewer: %v", err)
	}
	dup := sampleSpec("0c:ea:14:42:42:42", 8081)
	err := mgr.AddViewer(context.Background(), dup)
	if !errors.Is(err, ErrMACInUse) {
		t.Errorf("err = %v, want ErrMACInUse", err)
	}
}

func TestAddViewer_DuplicatePortError(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("first AddViewer: %v", err)
	}
	dup := sampleSpec("0c:ea:14:42:42:43", 8080)
	err := mgr.AddViewer(context.Background(), dup)
	if !errors.Is(err, ErrPortInUse) {
		t.Errorf("err = %v, want ErrPortInUse", err)
	}
}

func TestAddViewer_RejectsEmptyMAC(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := ViewerSpec{Name: "x", ServicePort: 8080}
	if err := mgr.AddViewer(context.Background(), spec); err == nil {
		t.Fatal("AddViewer with empty MAC returned nil")
	}
}

// ---------- RemoveViewer ----------

func TestRemoveViewer_StopsGoroutine(t *testing.T) {
	mgr, factory := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	v := factory.viewer(spec.MAC)
	for i := 0; i < 100 && v.runs.Load() != 1; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := mgr.RemoveViewer(ctx, spec.MAC); err != nil {
		t.Fatalf("RemoveViewer: %v", err)
	}
	select {
	case <-v.stopped:
	case <-time.After(2 * time.Second):
		t.Errorf("viewer Run did not stop after RemoveViewer")
	}
}

func TestRemoveViewer_DeletesFromDB(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	if err := mgr.RemoveViewer(context.Background(), spec.MAC); err != nil {
		t.Fatalf("RemoveViewer: %v", err)
	}
	var count int
	if err := mgr.db.QueryRow(
		`SELECT COUNT(*) FROM viewers WHERE mac = ?`, spec.MAC,
	).Scan(&count); err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 0 {
		t.Errorf("row count after Remove = %d, want 0", count)
	}
}

func TestRemoveViewer_UnknownReturnsError(t *testing.T) {
	mgr, _ := newTestManager(t)
	err := mgr.RemoveViewer(context.Background(), "0c:ea:14:99:99:99")
	if !errors.Is(err, ErrViewerNotFound) {
		t.Errorf("err = %v, want ErrViewerNotFound", err)
	}
}

func TestRemoveViewer_CascadesSessions(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:cc:dd:ee", 8082)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	now := time.Now().UnixMilli()
	if _, err := mgr.db.Exec(
		`INSERT INTO viewer_sessions (session_id, viewer_mac, created_at, last_seen, expires_at)
		 VALUES (?, ?, ?, ?, ?)`,
		"sess-x", spec.MAC, now, now, now,
	); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if err := mgr.RemoveViewer(context.Background(), spec.MAC); err != nil {
		t.Fatalf("RemoveViewer: %v", err)
	}
	var n int
	if err := mgr.db.QueryRow(
		`SELECT COUNT(*) FROM viewer_sessions WHERE viewer_mac = ?`, spec.MAC,
	).Scan(&n); err != nil {
		t.Fatalf("count viewer_sessions: %v", err)
	}
	if n != 0 {
		t.Errorf("viewer_sessions count after RemoveViewer = %d, want 0 (FK cascade)", n)
	}
}

// ---------- GetViewerInfo ----------

func TestGetViewerInfo_HappyPath(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	info, err := mgr.GetViewerInfo(context.Background(), spec.MAC)
	if err != nil {
		t.Fatalf("GetViewerInfo: %v", err)
	}
	if info.Name != spec.Name {
		t.Errorf("Name = %q, want %q", info.Name, spec.Name)
	}
	if info.ServicePort != spec.ServicePort {
		t.Errorf("ServicePort = %d, want %d", info.ServicePort, spec.ServicePort)
	}
	if !info.Running {
		t.Errorf("Running = false, want true")
	}
}

func TestGetViewerInfo_UnknownMAC(t *testing.T) {
	mgr, _ := newTestManager(t)
	_, err := mgr.GetViewerInfo(context.Background(), "0c:ea:14:99:99:99")
	if !errors.Is(err, ErrViewerNotFound) {
		t.Errorf("err = %v, want ErrViewerNotFound", err)
	}
}

// ---------- ListViewers ----------

func TestListViewers_ReturnsAll(t *testing.T) {
	mgr, _ := newTestManager(t)
	specs := []ViewerSpec{
		sampleSpec("0c:ea:14:42:42:42", 8080),
		sampleSpec("0c:ea:14:42:42:43", 8081),
		sampleSpec("0c:ea:14:42:42:44", 8082),
	}
	for _, s := range specs {
		if err := mgr.AddViewer(context.Background(), s); err != nil {
			t.Fatalf("AddViewer %s: %v", s.MAC, err)
		}
	}
	got, err := mgr.ListViewers(context.Background())
	if err != nil {
		t.Fatalf("ListViewers: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("ListViewers len = %d, want 3", len(got))
	}
	for _, info := range got {
		if !info.Running {
			t.Errorf("viewer %s reports Running=false", info.MAC)
		}
	}
}

// ---------- Shutdown ----------

// ---------- SetPairedIntercomMAC (saison-13-07) ----------

func TestSetPairedIntercomMAC_RoundTrip(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	if err := mgr.SetPairedIntercomMAC(context.Background(), spec.MAC, "28:70:4E:31:E2:9C"); err != nil {
		t.Fatalf("SetPairedIntercomMAC: %v", err)
	}
	info, err := mgr.GetViewerInfo(context.Background(), spec.MAC)
	if err != nil {
		t.Fatalf("GetViewerInfo: %v", err)
	}
	if info.PairedIntercomMAC != "28:70:4e:31:e2:9c" {
		t.Errorf("PairedIntercomMAC = %q, want %q (lowercase)",
			info.PairedIntercomMAC, "28:70:4e:31:e2:9c")
	}
}

func TestSetPairedIntercomMAC_ClearWithEmptyString(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	spec.PairedIntercomMAC = "28:70:4e:31:e2:9c"
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	if err := mgr.SetPairedIntercomMAC(context.Background(), spec.MAC, ""); err != nil {
		t.Fatalf("SetPairedIntercomMAC clear: %v", err)
	}
	info, _ := mgr.GetViewerInfo(context.Background(), spec.MAC)
	if info.PairedIntercomMAC != "" {
		t.Errorf("PairedIntercomMAC after clear = %q, want empty", info.PairedIntercomMAC)
	}
}

func TestSetPairedIntercomMAC_UnknownViewer(t *testing.T) {
	mgr, _ := newTestManager(t)
	err := mgr.SetPairedIntercomMAC(context.Background(), "0c:ea:14:00:00:00", "28:70:4e:31:e2:9c")
	if !errors.Is(err, ErrViewerNotFound) {
		t.Errorf("err = %v, want ErrViewerNotFound", err)
	}
}

func TestAddViewer_PersistsPairedIntercomMAC(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	spec.PairedIntercomMAC = "28:70:4E:31:E2:9C"
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	info, _ := mgr.GetViewerInfo(context.Background(), spec.MAC)
	if info.PairedIntercomMAC != "28:70:4e:31:e2:9c" {
		t.Errorf("PairedIntercomMAC = %q, want lowercase", info.PairedIntercomMAC)
	}
}

// ---------- Hybrid web+esp goroutine spawn (saison-13-09) ----------

// espSpec helpers an ESP-Spec with a token-hash placeholder so the
// row passes the discovery + AddViewer pipeline shape.
func espSpec(mac string, port uint16) ViewerSpec {
	return ViewerSpec{
		MAC:         mac,
		Name:        "esp-" + mac,
		ServicePort: port,
		Type:        TypeESP,
		ESPTokenHash: "deadbeefdeadbeefdeadbeefdeadbeef" +
			"deadbeefdeadbeefdeadbeefdeadbeef",
	}
}

func TestAddViewer_TypeESP_SpawnsGoroutine(t *testing.T) {
	mgr, factory := newTestManager(t)
	spec := espSpec("0c:ea:14:aa:bb:cc", 8200)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	if got := factory.starts.Load(); got != 1 {
		t.Fatalf("factory.starts = %d, want 1 (S13-09 hybrid spawn)", got)
	}
	v := factory.viewer(spec.MAC)
	if v == nil {
		t.Fatal("factory did not record an ESP viewer")
	}
	for i := 0; i < 100 && v.runs.Load() != 1; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if v.runs.Load() != 1 {
		t.Errorf("ESP viewer Run not invoked within 500ms")
	}
}

func TestRemoveViewer_TypeESP_StopsGoroutine(t *testing.T) {
	mgr, factory := newTestManager(t)
	spec := espSpec("0c:ea:14:aa:bb:cc", 8200)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	v := factory.viewer(spec.MAC)
	for i := 0; i < 100 && v.runs.Load() != 1; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := mgr.RemoveViewer(ctx, spec.MAC); err != nil {
		t.Fatalf("RemoveViewer: %v", err)
	}
	select {
	case <-v.stopped:
	case <-time.After(2 * time.Second):
		t.Errorf("ESP viewer Run did not stop after RemoveViewer")
	}
}

func TestSetESPTokenHash_KeepsGoroutineRunning(t *testing.T) {
	mgr, factory := newTestManager(t)
	spec := espSpec("0c:ea:14:aa:bb:cc", 8200)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	v := factory.viewer(spec.MAC)
	for i := 0; i < 100 && v.runs.Load() != 1; i++ {
		time.Sleep(5 * time.Millisecond)
	}

	// Token rotation: the admin "Token erneuern"-Button or the
	// /esp/discover/status handoff path. The viewer goroutine
	// MUST keep running - rotating only the DB-persisted hash
	// must not cancel the live UDM-Mock.
	const newHash = "0123456789abcdef0123456789abcdef" +
		"0123456789abcdef0123456789abcdef"
	if err := mgr.SetESPTokenHash(context.Background(), spec.MAC, newHash); err != nil {
		t.Fatalf("SetESPTokenHash: %v", err)
	}

	// Goroutine still running.
	if v.runs.Load() != 1 {
		t.Errorf("Run-count = %d after token rotation, want 1 still", v.runs.Load())
	}
	select {
	case <-v.stopped:
		t.Fatal("viewer.stopped fired - goroutine was killed by token rotation")
	default:
	}

	// And the new hash actually landed in the DB.
	got, err := mgr.LookupESPTokenHash(context.Background(), spec.MAC)
	if err != nil {
		t.Fatalf("LookupESPTokenHash: %v", err)
	}
	if got != newHash {
		t.Errorf("stored hash = %q, want %q", got, newHash)
	}
}

func TestLoadFromDB_StartsBothWebAndESPViewers(t *testing.T) {
	mgr, factory := newTestManager(t)
	now := mgr.opts.Now().UnixMilli()
	rows := []struct {
		mac, kind string
		port      int64
	}{
		{"0c:ea:14:42:42:42", "web", 8080},
		{"0c:ea:14:aa:bb:cc", "esp", 8200},
	}
	for _, r := range rows {
		if _, err := mgr.db.Exec(
			`INSERT INTO viewers (mac, name, service_port, type, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			r.mac, "viewer-"+r.mac, r.port, r.kind, now, now,
		); err != nil {
			t.Fatalf("seed %s: %v", r.kind, err)
		}
	}
	if err := mgr.LoadFromDB(context.Background()); err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}
	if got := factory.starts.Load(); got != 2 {
		t.Fatalf("factory.starts = %d, want 2 (web + esp)", got)
	}
	infos, _ := mgr.ListViewers(context.Background())
	if len(infos) != 2 {
		t.Errorf("ListViewers len = %d, want 2", len(infos))
	}
	// And each one shows Running=true (came from the in-memory map).
	for _, info := range infos {
		if !info.Running {
			t.Errorf("viewer %s (%s) Running=false, want true", info.MAC, info.Type)
		}
	}
}

// ---------- Saison 14-XX ESP-Settings ----------

func TestSetIdleViewMode_AcceptsScreenOff(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	if err := mgr.SetIdleViewMode(context.Background(), spec.MAC, IdleViewModeScreenOff); err != nil {
		t.Fatalf("SetIdleViewMode(screen_off): %v", err)
	}
	info, err := mgr.GetViewerInfo(context.Background(), spec.MAC)
	if err != nil {
		t.Fatalf("GetViewerInfo: %v", err)
	}
	if info.IdleViewMode != IdleViewModeScreenOff {
		t.Errorf("IdleViewMode = %q, want %q", info.IdleViewMode, IdleViewModeScreenOff)
	}
	if info.ResolveIdleViewMode() != IdleViewModeScreenOff {
		t.Errorf("ResolveIdleViewMode = %q, want %q",
			info.ResolveIdleViewMode(), IdleViewModeScreenOff)
	}
}

func TestSetIdleViewMode_RejectsUnknownValue(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	if err := mgr.SetIdleViewMode(context.Background(), spec.MAC, "bogus"); err == nil {
		t.Error("SetIdleViewMode bogus returned nil error")
	}
}

func TestSetBrightnessIdle_RoundTrip(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	if err := mgr.SetBrightnessIdle(context.Background(), spec.MAC, 42); err != nil {
		t.Fatalf("SetBrightnessIdle: %v", err)
	}
	info, _ := mgr.GetViewerInfo(context.Background(), spec.MAC)
	if got := info.ResolveBrightnessIdle(); got != 42 {
		t.Errorf("ResolveBrightnessIdle = %d, want 42", got)
	}
}

func TestSetBrightnessIdle_RejectsOutOfRange(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	for _, bad := range []int{-1, 101, 200} {
		if err := mgr.SetBrightnessIdle(context.Background(), spec.MAC, bad); err == nil {
			t.Errorf("SetBrightnessIdle(%d) returned nil error", bad)
		}
	}
}

func TestResolveBrightnessIdle_DefaultsWhenNull(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	info, _ := mgr.GetViewerInfo(context.Background(), spec.MAC)
	if got := info.ResolveBrightnessIdle(); got != DefaultBrightnessIdle {
		t.Errorf("ResolveBrightnessIdle = %d, want %d (default)", got, DefaultBrightnessIdle)
	}
}

func TestSetScreenOffAfterSec_RoundTripAndDisable(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	if err := mgr.SetScreenOffAfterSec(context.Background(), spec.MAC, 300); err != nil {
		t.Fatalf("SetScreenOffAfterSec(300): %v", err)
	}
	info, _ := mgr.GetViewerInfo(context.Background(), spec.MAC)
	if got := info.ResolveScreenOffAfterSec(); got != 300 {
		t.Errorf("ResolveScreenOffAfterSec = %d, want 300", got)
	}
	if err := mgr.SetScreenOffAfterSec(context.Background(), spec.MAC, 0); err != nil {
		t.Fatalf("SetScreenOffAfterSec(0): %v", err)
	}
	info, _ = mgr.GetViewerInfo(context.Background(), spec.MAC)
	if got := info.ResolveScreenOffAfterSec(); got != 0 {
		t.Errorf("after disable = %d, want 0", got)
	}
}

func TestSetScreenOffAfterSec_RejectsUnknownValue(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	if err := mgr.SetScreenOffAfterSec(context.Background(), spec.MAC, 999); err == nil {
		t.Error("SetScreenOffAfterSec(999) returned nil error")
	}
}

func TestSetLanguage_RoundTripAndDefault(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	if err := mgr.SetLanguage(context.Background(), spec.MAC, "en"); err != nil {
		t.Fatalf("SetLanguage(en): %v", err)
	}
	info, _ := mgr.GetViewerInfo(context.Background(), spec.MAC)
	if got := info.ResolveLanguage(); got != "en" {
		t.Errorf("ResolveLanguage = %q, want en", got)
	}
	if err := mgr.SetLanguage(context.Background(), spec.MAC, ""); err != nil {
		t.Fatalf("SetLanguage(empty): %v", err)
	}
	info, _ = mgr.GetViewerInfo(context.Background(), spec.MAC)
	if got := info.ResolveLanguage(); got != DefaultLanguage {
		t.Errorf("after clear = %q, want %q (default)", got, DefaultLanguage)
	}
}

func TestHistoryCaptureEnabled_DefaultsTrueWhenNull(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	info, _ := mgr.GetViewerInfo(context.Background(), spec.MAC)
	// Saison-14-04-Phase2-Migration setzt DEFAULT 1; ein neu
	// angelegter Viewer hat damit immer capture=true.
	if !info.ResolveHistoryCaptureEnabled() {
		t.Errorf("ResolveHistoryCaptureEnabled() = false, want true (default)")
	}
}

func TestSetHistoryCaptureEnabled_RoundTrip(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	if err := mgr.SetHistoryCaptureEnabled(context.Background(), spec.MAC, false); err != nil {
		t.Fatalf("Set false: %v", err)
	}
	info, _ := mgr.GetViewerInfo(context.Background(), spec.MAC)
	if info.ResolveHistoryCaptureEnabled() {
		t.Errorf("after Set(false) = true, want false")
	}
	if err := mgr.SetHistoryCaptureEnabled(context.Background(), spec.MAC, true); err != nil {
		t.Fatalf("Set true: %v", err)
	}
	info, _ = mgr.GetViewerInfo(context.Background(), spec.MAC)
	if !info.ResolveHistoryCaptureEnabled() {
		t.Errorf("after Set(true) = false, want true")
	}
}

func TestSetHistoryCaptureEnabled_UnknownViewer(t *testing.T) {
	mgr, _ := newTestManager(t)
	err := mgr.SetHistoryCaptureEnabled(context.Background(), "0c:ea:14:00:00:00", false)
	if !errors.Is(err, ErrViewerNotFound) {
		t.Errorf("err = %v, want ErrViewerNotFound", err)
	}
}

// ---------- Saison 14-04-Phase2-FIX05 clock_layout ----------

func TestClockLayout_DefaultsToVerticalWhenNull(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	info, _ := mgr.GetViewerInfo(context.Background(), spec.MAC)
	if info.ResolveClockLayout() != ClockLayoutVertical {
		t.Errorf("default = %q, want %q (vertical)",
			info.ResolveClockLayout(), ClockLayoutVertical)
	}
}

func TestSetClockLayout_RoundTrip(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	if err := mgr.SetClockLayout(context.Background(), spec.MAC, ClockLayoutHorizontal); err != nil {
		t.Fatalf("SetClockLayout(horizontal): %v", err)
	}
	info, _ := mgr.GetViewerInfo(context.Background(), spec.MAC)
	if info.ResolveClockLayout() != ClockLayoutHorizontal {
		t.Errorf("after Set(horizontal) = %q", info.ResolveClockLayout())
	}
	if err := mgr.SetClockLayout(context.Background(), spec.MAC, ClockLayoutVertical); err != nil {
		t.Fatalf("SetClockLayout(vertical): %v", err)
	}
	info, _ = mgr.GetViewerInfo(context.Background(), spec.MAC)
	if info.ResolveClockLayout() != ClockLayoutVertical {
		t.Errorf("after Set(vertical) = %q", info.ResolveClockLayout())
	}
}

func TestSetClockLayout_RejectsUnknown(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	if err := mgr.SetClockLayout(context.Background(), spec.MAC, "diagonal"); err == nil {
		t.Error("SetClockLayout(diagonal) returned nil error")
	}
}

func TestSetClockLayout_UnknownViewer(t *testing.T) {
	mgr, _ := newTestManager(t)
	err := mgr.SetClockLayout(context.Background(), "0c:ea:14:00:00:00", ClockLayoutHorizontal)
	if !errors.Is(err, ErrViewerNotFound) {
		t.Errorf("err = %v, want ErrViewerNotFound", err)
	}
}

func TestSetLanguage_RejectsUnknown(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:42:42:42", 8080)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	if err := mgr.SetLanguage(context.Background(), spec.MAC, "fr"); err == nil {
		t.Error("SetLanguage(fr) returned nil error")
	}
}

func TestShutdown_StopsAllViewers(t *testing.T) {
	mgr, factory := newTestManager(t)
	for _, s := range []ViewerSpec{
		sampleSpec("0c:ea:14:42:42:42", 8080),
		sampleSpec("0c:ea:14:42:42:43", 8081),
	} {
		if err := mgr.AddViewer(context.Background(), s); err != nil {
			t.Fatalf("AddViewer %s: %v", s.MAC, err)
		}
	}
	for _, mac := range []string{"0c:ea:14:42:42:42", "0c:ea:14:42:42:43"} {
		v := factory.viewer(mac)
		for i := 0; i < 100 && v.runs.Load() != 1; i++ {
			time.Sleep(5 * time.Millisecond)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := mgr.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	for _, mac := range []string{"0c:ea:14:42:42:42", "0c:ea:14:42:42:43"} {
		select {
		case <-factory.viewer(mac).stopped:
		case <-time.After(time.Second):
			t.Errorf("viewer %s did not stop", mac)
		}
	}
}
