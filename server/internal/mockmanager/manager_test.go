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

	"unifix.local/mock"
	"unifix.local/server/internal/db"
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
			`INSERT INTO mock_viewers (mac, name, service_port, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?)`,
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
		`SELECT name, service_port FROM mock_viewers WHERE mac = ?`, spec.MAC,
	).Scan(&name, &port)
	if err != nil {
		t.Fatalf("query mock_viewers: %v", err)
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
		`SELECT COUNT(*) FROM mock_viewers WHERE mac = ?`, spec.MAC,
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

func TestRemoveViewer_CascadesSessionsAndTokens(t *testing.T) {
	mgr, _ := newTestManager(t)
	spec := sampleSpec("0c:ea:14:cc:dd:ee", 8082)
	if err := mgr.AddViewer(context.Background(), spec); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	now := time.Now().UnixMilli()
	if _, err := mgr.db.Exec(
		`INSERT INTO mieter_sessions (session_id, mock_mac, created_at, last_seen, expires_at)
		 VALUES (?, ?, ?, ?, ?)`,
		"sess-x", spec.MAC, now, now, now,
	); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err := mgr.db.Exec(
		`INSERT INTO magic_link_tokens (token, mock_mac, created_at, expires_at)
		 VALUES (?, ?, ?, ?)`,
		"tok-x", spec.MAC, now, now,
	); err != nil {
		t.Fatalf("insert token: %v", err)
	}
	if err := mgr.RemoveViewer(context.Background(), spec.MAC); err != nil {
		t.Fatalf("RemoveViewer: %v", err)
	}
	for _, q := range []struct {
		label string
		sql   string
	}{
		{"mieter_sessions", `SELECT COUNT(*) FROM mieter_sessions WHERE mock_mac = ?`},
		{"magic_link_tokens", `SELECT COUNT(*) FROM magic_link_tokens WHERE mock_mac = ?`},
	} {
		var n int
		if err := mgr.db.QueryRow(q.sql, spec.MAC).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", q.label, err)
		}
		if n != 0 {
			t.Errorf("%s count after RemoveViewer = %d, want 0 (FK cascade)", q.label, n)
		}
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
