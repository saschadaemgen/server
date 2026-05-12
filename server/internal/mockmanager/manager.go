// Package mockmanager owns the lifecycle of embedded mock
// viewers inside unifix-server. Each viewer runs as a goroutine
// hosted by the server process; the manager loads persisted
// viewer specs from the mock_viewers table on boot, starts the
// goroutines, multiplexes their event channels, and handles
// admin-driven add/remove operations.
//
// Saison 12-06 refactor: ua_user_id is gone. Mock-MAC is the
// only routing key. Tenants reach a mock via a magic-link the
// admin generates; the link binds the resulting browser session
// to mock_mac, not to a user.
//
// The manager exposes a Viewer interface and a ViewerFactory so
// tests can inject a fake viewer instead of spinning up the real
// mock stack against a non-existent UDM.
package mockmanager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"unifix.local/mock"
	"unifix.local/server/internal/db"
)

// Channel buffer for the multiplexed event streams. The manager
// drops on overflow, like the per-viewer channels.
const (
	multiplexEventBuffer  = 64
	multiplexCancelBuffer = 64
)

// Sentinel errors. Callers check via errors.Is.
var (
	ErrMACInUse       = errors.New("mockmanager: mac already registered")
	ErrPortInUse      = errors.New("mockmanager: service_port already registered")
	ErrViewerNotFound = errors.New("mockmanager: viewer not found")
)

// Viewer is the subset of mock.Viewer that Manager needs. Defined
// as an interface so tests can inject a fake.
type Viewer interface {
	Run(ctx context.Context) error
	Events() <-chan mock.DoorbellEvent
	Cancels() <-chan mock.DoorbellCancelEvent
	MAC() string
}

// ViewerFactory constructs a Viewer for the given config.
type ViewerFactory func(cfg mock.Config, log *slog.Logger) (Viewer, error)

// DefaultFactory wraps mock.New and returns the resulting viewer
// as a Viewer interface. Production use.
func DefaultFactory(cfg mock.Config, log *slog.Logger) (Viewer, error) {
	return mock.New(cfg, log)
}

// ViewerSpec describes one persisted mock viewer.
type ViewerSpec struct {
	MAC         string
	Name        string
	ServicePort uint16
}

// ViewerInfo is the public view of one running mock viewer for
// the admin UI.
type ViewerInfo struct {
	MAC         string
	Name        string
	ServicePort uint16
	Running     bool
}

// Options configures Manager construction.
type Options struct {
	// StateDirBase is the parent directory passed to every
	// viewer's Config.StateDir. Each viewer creates its own
	// sub-directory under it.
	StateDirBase string

	// ServerIPv4 is the IPv4 the viewers announce in their
	// discovery replies. Must be set for embedded viewers to be
	// reachable by UDM.
	ServerIPv4 string

	// Factory builds viewers from configs. Nil falls back to
	// DefaultFactory; tests override it.
	Factory ViewerFactory

	// Now is the clock source. Nil falls back to time.Now;
	// tests inject deterministic clocks.
	Now func() time.Time
}

// Manager runs and supervises a collection of mock viewers.
type Manager struct {
	db   *db.DB
	log  *slog.Logger
	opts Options

	mu      sync.Mutex
	viewers map[string]*viewerEntry

	eventCh  chan mock.DoorbellEvent
	cancelCh chan mock.DoorbellCancelEvent

	wg sync.WaitGroup
}

type viewerEntry struct {
	spec   ViewerSpec
	viewer Viewer
	cancel context.CancelFunc
	done   chan struct{}
}

// New constructs a Manager. The Manager starts no viewers until
// LoadFromDB or AddViewer is called.
func New(d *db.DB, log *slog.Logger, opts Options) *Manager {
	if log == nil {
		log = slog.Default()
	}
	if opts.Factory == nil {
		opts.Factory = DefaultFactory
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Manager{
		db:       d,
		log:      log.With("component", "mockmanager"),
		opts:     opts,
		viewers:  make(map[string]*viewerEntry),
		eventCh:  make(chan mock.DoorbellEvent, multiplexEventBuffer),
		cancelCh: make(chan mock.DoorbellCancelEvent, multiplexCancelBuffer),
	}
}

// Events returns the multiplexed channel of doorbell events
// from every running viewer.
func (m *Manager) Events() <-chan mock.DoorbellEvent { return m.eventCh }

// Cancels returns the multiplexed channel of doorbell cancels.
func (m *Manager) Cancels() <-chan mock.DoorbellCancelEvent { return m.cancelCh }

// LoadFromDB reads every row from mock_viewers and starts a
// goroutine per row. Called once at server boot.
func (m *Manager) LoadFromDB(ctx context.Context) error {
	rows, err := m.db.QueryContext(ctx,
		`SELECT mac, name, service_port FROM mock_viewers ORDER BY mac`)
	if err != nil {
		return fmt.Errorf("mockmanager: load: %w", err)
	}
	defer rows.Close()

	specs := make([]ViewerSpec, 0)
	for rows.Next() {
		var spec ViewerSpec
		var port int64
		if err := rows.Scan(&spec.MAC, &spec.Name, &port); err != nil {
			return fmt.Errorf("mockmanager: scan: %w", err)
		}
		spec.ServicePort = uint16(port)
		specs = append(specs, spec)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("mockmanager: rows: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, spec := range specs {
		if err := m.startViewerLocked(spec); err != nil {
			m.log.Error("start viewer failed during load",
				"mac", spec.MAC, "err", err)
			continue
		}
	}
	m.log.Info("loaded mock viewers", "count", len(specs))
	return nil
}

// AddViewer registers a new mock viewer: persists it to mock_viewers
// then spawns its goroutine. Returns ErrMACInUse or ErrPortInUse on
// collision with an already-running viewer.
func (m *Manager) AddViewer(ctx context.Context, spec ViewerSpec) error {
	if err := validateSpec(spec); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.viewers[spec.MAC]; exists {
		return ErrMACInUse
	}
	for _, e := range m.viewers {
		if e.spec.ServicePort == spec.ServicePort {
			return ErrPortInUse
		}
	}

	if err := m.insertViewerLocked(ctx, spec); err != nil {
		return err
	}

	if err := m.startViewerLocked(spec); err != nil {
		// Best-effort rollback: drop the row so the next call
		// is not blocked by a phantom entry.
		_, _ = m.db.ExecContext(ctx, `DELETE FROM mock_viewers WHERE mac = ?`, spec.MAC)
		return err
	}
	return nil
}

// RemoveViewer cancels the viewer goroutine, waits for it to
// stop (or for ctx to expire), then deletes the row. The
// foreign-key cascade in the schema sweeps any magic_link_tokens
// and mieter_sessions bound to this mock with the same DELETE.
func (m *Manager) RemoveViewer(ctx context.Context, mac string) error {
	m.mu.Lock()
	entry, ok := m.viewers[mac]
	if !ok {
		m.mu.Unlock()
		return ErrViewerNotFound
	}
	delete(m.viewers, mac)
	m.mu.Unlock()

	entry.cancel()
	select {
	case <-entry.done:
	case <-ctx.Done():
	}

	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM mock_viewers WHERE mac = ?`, mac,
	); err != nil {
		return fmt.Errorf("mockmanager: delete: %w", err)
	}
	return nil
}

// GetViewerInfo returns the snapshot for one running viewer by
// MAC, or ErrViewerNotFound if the MAC is unknown.
func (m *Manager) GetViewerInfo(_ context.Context, mac string) (*ViewerInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.viewers[mac]
	if !ok {
		return nil, ErrViewerNotFound
	}
	return &ViewerInfo{
		MAC:         e.spec.MAC,
		Name:        e.spec.Name,
		ServicePort: e.spec.ServicePort,
		Running:     true,
	}, nil
}

// ListViewers returns the snapshot of currently registered viewers.
func (m *Manager) ListViewers(_ context.Context) ([]ViewerInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ViewerInfo, 0, len(m.viewers))
	for _, e := range m.viewers {
		out = append(out, ViewerInfo{
			MAC:         e.spec.MAC,
			Name:        e.spec.Name,
			ServicePort: e.spec.ServicePort,
			Running:     true,
		})
	}
	return out, nil
}

// Shutdown cancels every viewer and waits for the goroutines to
// finish (or ctx to expire).
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	entries := make([]*viewerEntry, 0, len(m.viewers))
	for mac, e := range m.viewers {
		entries = append(entries, e)
		delete(m.viewers, mac)
	}
	m.mu.Unlock()

	for _, e := range entries {
		e.cancel()
	}

	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// --- internal helpers ---

func (m *Manager) insertViewerLocked(ctx context.Context, spec ViewerSpec) error {
	now := m.opts.Now().UnixMilli()
	_, err := m.db.ExecContext(ctx,
		`INSERT INTO mock_viewers (mac, name, service_port, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		spec.MAC, spec.Name, int64(spec.ServicePort), now, now,
	)
	if err != nil {
		return fmt.Errorf("mockmanager: insert: %w", err)
	}
	return nil
}

func (m *Manager) startViewerLocked(spec ViewerSpec) error {
	cfg := mock.Config{
		MAC:         spec.MAC,
		IPv4:        m.opts.ServerIPv4,
		Name:        spec.Name,
		ServicePort: spec.ServicePort,
		StateDir:    m.opts.StateDirBase,
	}
	viewer, err := m.opts.Factory(cfg, m.log)
	if err != nil {
		return fmt.Errorf("mockmanager: factory: %w", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	entry := &viewerEntry{
		spec:   spec,
		viewer: viewer,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	m.viewers[spec.MAC] = entry

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(entry.done)
		if err := viewer.Run(runCtx); err != nil && !errors.Is(err, context.Canceled) {
			m.log.Error("viewer run failed", "mac", spec.MAC, "err", err)
		}
	}()

	m.wg.Add(2)
	go m.forwardEvents(runCtx, viewer.Events())
	go m.forwardCancels(runCtx, viewer.Cancels())

	return nil
}

func (m *Manager) forwardEvents(ctx context.Context, src <-chan mock.DoorbellEvent) {
	defer m.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-src:
			if !ok {
				return
			}
			select {
			case <-ctx.Done():
				return
			case m.eventCh <- ev:
			default:
				m.log.Warn("multiplex event channel full, dropping",
					"mac", ev.MockMAC,
					"request_id", ev.RequestID,
				)
			}
		}
	}
}

func (m *Manager) forwardCancels(ctx context.Context, src <-chan mock.DoorbellCancelEvent) {
	defer m.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-src:
			if !ok {
				return
			}
			select {
			case <-ctx.Done():
				return
			case m.cancelCh <- ev:
			default:
				m.log.Warn("multiplex cancel channel full, dropping",
					"mac", ev.MockMAC,
					"cancel_token", ev.CancelToken,
				)
			}
		}
	}
}

func validateSpec(spec ViewerSpec) error {
	if spec.MAC == "" {
		return errors.New("mockmanager: MAC must not be empty")
	}
	if spec.Name == "" {
		return errors.New("mockmanager: Name must not be empty")
	}
	if spec.ServicePort == 0 {
		return errors.New("mockmanager: ServicePort must be > 0")
	}
	return nil
}
