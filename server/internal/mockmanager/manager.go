// Package mockmanager owns the lifecycle of embedded mock viewers
// inside unifix-server. Each viewer runs as a goroutine hosted by
// the server process; the manager loads persisted specs from the
// viewers table on boot, starts the goroutines, multiplexes their
// event channels, and handles admin-driven add / remove operations.
//
// Saison 13-02-FIX4-a: the persistence table is now `viewers`
// (was mock_viewers) and rows can be of type 'web' or 'esp'. The
// manager only spawns goroutines for type 'web'; ESP viewers are
// authenticated separately and run on real hardware.
//
// The manager exposes a Viewer interface and a ViewerFactory so
// tests can inject a fake viewer instead of spinning up the real
// mock stack against a non-existent UDM.
package mockmanager

import (
	"context"
	"database/sql"
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

// Viewer-Type-Konstanten (Spalte viewers.type).
const (
	TypeWeb = "web"
	TypeESP = "esp"
)

// Sentinel errors. Callers check via errors.Is.
var (
	ErrMACInUse       = errors.New("mockmanager: mac already registered")
	ErrPortInUse      = errors.New("mockmanager: service_port already registered")
	ErrViewerNotFound = errors.New("mockmanager: viewer not found")
	ErrUsernameInUse  = errors.New("mockmanager: username already in use")
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

// ViewerSpec describes one persisted viewer.
type ViewerSpec struct {
	MAC         string
	Name        string
	ServicePort uint16
	Type        string // TypeWeb / TypeESP. Empty defaults to TypeWeb.
	Username    string
}

// ViewerInfo is the public view of one running viewer for the
// admin UI.
type ViewerInfo struct {
	MAC           string
	Name          string
	ServicePort   uint16
	Type          string
	Username      string
	HasPassword   bool
	PasswordSetAt *time.Time
	Running       bool
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

// Manager runs and supervises a collection of viewers.
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

// Events returns the multiplexed channel of doorbell events from
// every running viewer.
func (m *Manager) Events() <-chan mock.DoorbellEvent { return m.eventCh }

// Cancels returns the multiplexed channel of doorbell cancels.
func (m *Manager) Cancels() <-chan mock.DoorbellCancelEvent { return m.cancelCh }

// LoadFromDB reads every web-type row from viewers and starts a
// goroutine per row. ESP rows are skipped (handled separately).
// Called once at server boot.
func (m *Manager) LoadFromDB(ctx context.Context) error {
	rows, err := m.db.QueryContext(ctx,
		`SELECT mac, name, service_port, type, COALESCE(username, '')
		   FROM viewers
		  WHERE type = ?
		  ORDER BY mac`, TypeWeb)
	if err != nil {
		return fmt.Errorf("mockmanager: load: %w", err)
	}
	defer rows.Close()

	specs := make([]ViewerSpec, 0)
	for rows.Next() {
		var spec ViewerSpec
		var port int64
		if err := rows.Scan(&spec.MAC, &spec.Name, &port, &spec.Type, &spec.Username); err != nil {
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
	m.log.Info("loaded web viewers", "count", len(specs))
	return nil
}

// AddViewer registers a new viewer: persists it to viewers then
// spawns its goroutine. Returns ErrMACInUse, ErrPortInUse or
// ErrUsernameInUse on collision with an existing row.
func (m *Manager) AddViewer(ctx context.Context, spec ViewerSpec) error {
	if err := validateSpec(spec); err != nil {
		return err
	}
	if spec.Type == "" {
		spec.Type = TypeWeb
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
		if spec.Username != "" && e.spec.Username == spec.Username {
			return ErrUsernameInUse
		}
	}

	if err := m.insertViewerLocked(ctx, spec); err != nil {
		return err
	}

	if spec.Type == TypeWeb {
		if err := m.startViewerLocked(spec); err != nil {
			// Best-effort rollback: drop the row so the next call
			// is not blocked by a phantom entry.
			_, _ = m.db.ExecContext(ctx, `DELETE FROM viewers WHERE mac = ?`, spec.MAC)
			return err
		}
	}
	return nil
}

// RemoveViewer cancels the viewer goroutine, waits for it to
// stop (or for ctx to expire), then deletes the row. The
// foreign-key cascade in the schema sweeps any viewer_sessions
// bound to this viewer with the same DELETE.
func (m *Manager) RemoveViewer(ctx context.Context, mac string) error {
	m.mu.Lock()
	entry, ok := m.viewers[mac]
	m.mu.Unlock()

	// Cancel running goroutine if any (ESP rows have none).
	if ok {
		m.mu.Lock()
		delete(m.viewers, mac)
		m.mu.Unlock()
		entry.cancel()
		select {
		case <-entry.done:
		case <-ctx.Done():
		}
	}

	res, err := m.db.ExecContext(ctx,
		`DELETE FROM viewers WHERE mac = ?`, mac)
	if err != nil {
		return fmt.Errorf("mockmanager: delete: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 && !ok {
		return ErrViewerNotFound
	}
	return nil
}

// Rename updates the viewer's display name in-place.
func (m *Manager) Rename(ctx context.Context, mac, newName string) error {
	if newName == "" {
		return errors.New("mockmanager: name must not be empty")
	}
	now := m.opts.Now().UnixMilli()
	res, err := m.db.ExecContext(ctx,
		`UPDATE viewers SET name = ?, updated_at = ? WHERE mac = ?`,
		newName, now, mac)
	if err != nil {
		return fmt.Errorf("mockmanager: rename: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrViewerNotFound
	}
	m.mu.Lock()
	if entry, ok := m.viewers[mac]; ok {
		entry.spec.Name = newName
	}
	m.mu.Unlock()
	return nil
}

// SetPasswordHash stores the Argon2id PHC string and stamps
// password_set_at.
func (m *Manager) SetPasswordHash(ctx context.Context, mac, hash string) error {
	now := m.opts.Now().UnixMilli()
	res, err := m.db.ExecContext(ctx,
		`UPDATE viewers SET password_hash = ?, password_set_at = ?, updated_at = ?
		 WHERE mac = ?`,
		hash, now, now, mac)
	if err != nil {
		return fmt.Errorf("mockmanager: set password: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrViewerNotFound
	}
	return nil
}

// LookupByUsername returns the viewer record for the given
// username (web-type only). Used by the viewer-login handler.
//
// Saison 13-02-FIX4-a-HOTFIX3: exact-match lookup. Migration 007
// normalisiert die viewers.username-Spalte, und der Caller (Login-
// Handler) jagt seine Eingabe durch dasselbe sanitizeUsername,
// das auch beim Anlegen lief. Ergebnis: kein LOWER mehr noetig,
// die Indizes greifen sauber und Umlaute / Mixed-Case passen
// symmetrisch.
func (m *Manager) LookupByUsername(ctx context.Context, username string) (*ViewerInfo, string, error) {
	var (
		info       ViewerInfo
		port       int64
		usernameDB sql.NullString
		hash       sql.NullString
		setAt      sql.NullInt64
	)
	err := m.db.QueryRowContext(ctx,
		`SELECT mac, name, service_port, type, username, password_hash, password_set_at
		   FROM viewers
		  WHERE username = ? AND type = 'web'`, username).
		Scan(&info.MAC, &info.Name, &port, &info.Type, &usernameDB, &hash, &setAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrViewerNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("mockmanager: lookup username: %w", err)
	}
	info.ServicePort = uint16(port)
	if usernameDB.Valid {
		info.Username = usernameDB.String
	}
	if hash.Valid {
		info.HasPassword = true
	}
	if setAt.Valid {
		t := time.UnixMilli(setAt.Int64)
		info.PasswordSetAt = &t
	}
	m.mu.Lock()
	if _, ok := m.viewers[info.MAC]; ok {
		info.Running = true
	}
	m.mu.Unlock()
	return &info, hashOrEmpty(hash), nil
}

// GetViewerInfo returns the snapshot for one viewer by MAC, or
// ErrViewerNotFound if the MAC is unknown.
func (m *Manager) GetViewerInfo(ctx context.Context, mac string) (*ViewerInfo, error) {
	info, err := m.loadInfo(ctx, mac)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if _, ok := m.viewers[info.MAC]; ok {
		info.Running = true
	}
	m.mu.Unlock()
	return info, nil
}

func (m *Manager) loadInfo(ctx context.Context, mac string) (*ViewerInfo, error) {
	var (
		info       ViewerInfo
		port       int64
		usernameDB sql.NullString
		hash       sql.NullString
		setAt      sql.NullInt64
	)
	err := m.db.QueryRowContext(ctx,
		`SELECT mac, name, service_port, type, username, password_hash, password_set_at
		   FROM viewers WHERE mac = ?`, mac).
		Scan(&info.MAC, &info.Name, &port, &info.Type, &usernameDB, &hash, &setAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrViewerNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("mockmanager: load info: %w", err)
	}
	info.ServicePort = uint16(port)
	if usernameDB.Valid {
		info.Username = usernameDB.String
	}
	if hash.Valid {
		info.HasPassword = true
	}
	if setAt.Valid {
		t := time.UnixMilli(setAt.Int64)
		info.PasswordSetAt = &t
	}
	return &info, nil
}

// ListViewers returns the snapshot of every persisted viewer
// (web + esp). Reads from the DB so esp-type rows are also
// surfaced; running flag comes from the in-memory map.
func (m *Manager) ListViewers(ctx context.Context) ([]ViewerInfo, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT mac, name, service_port, type, username, password_hash, password_set_at
		   FROM viewers ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("mockmanager: list: %w", err)
	}
	defer rows.Close()
	out := make([]ViewerInfo, 0)
	for rows.Next() {
		var (
			info       ViewerInfo
			port       int64
			usernameDB sql.NullString
			hash       sql.NullString
			setAt      sql.NullInt64
		)
		if err := rows.Scan(&info.MAC, &info.Name, &port, &info.Type,
			&usernameDB, &hash, &setAt); err != nil {
			return nil, fmt.Errorf("mockmanager: scan list: %w", err)
		}
		info.ServicePort = uint16(port)
		if usernameDB.Valid {
			info.Username = usernameDB.String
		}
		if hash.Valid {
			info.HasPassword = true
		}
		if setAt.Valid {
			t := time.UnixMilli(setAt.Int64)
			info.PasswordSetAt = &t
		}
		out = append(out, info)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mockmanager: list rows: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range out {
		if _, ok := m.viewers[out[i].MAC]; ok {
			out[i].Running = true
		}
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
		`INSERT INTO viewers
		   (mac, name, service_port, type, username, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		spec.MAC, spec.Name, int64(spec.ServicePort),
		spec.Type, nullable(spec.Username), now, now,
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
	if spec.Type != "" && spec.Type != TypeWeb && spec.Type != TypeESP {
		return fmt.Errorf("mockmanager: Type %q must be 'web' or 'esp'", spec.Type)
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func hashOrEmpty(h sql.NullString) string {
	if h.Valid {
		return h.String
	}
	return ""
}
