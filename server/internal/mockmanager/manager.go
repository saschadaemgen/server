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
	ErrNameInUse      = errors.New("mockmanager: viewer name already in use")
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
//
// Saison 13-02-FIX4-a-HOTFIX4: Username-Slot ist abgeschafft;
// der Wohnungs-Name ist der Login.
// Saison 13-02-FIX4-c: ESPModel / ESPFwVersion / ESPTokenHash
// werden nur bei Type='esp' beachtet. Bei TypeWeb bleiben sie
// leer.
type ViewerSpec struct {
	MAC            string
	Name           string
	ServicePort    uint16
	Type           string // TypeWeb / TypeESP. Empty defaults to TypeWeb.
	LinkedUAUserID string // optional UA-Access-User-Verknuepfung
	ESPModel       string
	ESPFwVersion   string
	ESPTokenHash   string
}

// ViewerInfo is the public view of one running viewer for the
// admin UI.
type ViewerInfo struct {
	MAC            string
	Name           string
	ServicePort    uint16
	Type           string
	HasPassword    bool
	PasswordSetAt  *time.Time
	LinkedUAUserID string
	ESPModel       string
	ESPFwVersion   string
	HasESPToken    bool
	Running        bool
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
		`SELECT mac, name, service_port, type,
		        COALESCE(linked_ua_user_id, '')
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
		if err := rows.Scan(&spec.MAC, &spec.Name, &port, &spec.Type, &spec.LinkedUAUserID); err != nil {
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
// ErrNameInUse on collision with an existing row.
//
// Saison 13-02-FIX4-a-HOTFIX4: Name-Uniqueness ist normalisiert
// (case + Umlaute + Whitespace), damit zwei Eintraege "Familie
// Mueller" und "FAMILIE MUELLER" als Duplikat erkannt werden.
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
	specKey := NormalizeName(spec.Name)
	for _, e := range m.viewers {
		if e.spec.ServicePort == spec.ServicePort {
			return ErrPortInUse
		}
		if NormalizeName(e.spec.Name) == specKey {
			return ErrNameInUse
		}
	}
	// In-Memory hat nur die laufenden web-type-Viewer. ESP-Eintraege
	// und vor LoadFromDB persistierte Reihen muessen direkt aus der
	// DB geprueft werden.
	if exists, err := m.nameExistsLocked(ctx, specKey, spec.MAC); err != nil {
		return err
	} else if exists {
		return ErrNameInUse
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

// Rename updates the viewer's display name in-place. Doppelte
// normalisierte Namen werden zurueckgewiesen (ErrNameInUse).
func (m *Manager) Rename(ctx context.Context, mac, newName string) error {
	if newName == "" {
		return errors.New("mockmanager: name must not be empty")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	exists, err := m.nameExistsLocked(ctx, NormalizeName(newName), mac)
	if err != nil {
		return err
	}
	if exists {
		return ErrNameInUse
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
	if entry, ok := m.viewers[mac]; ok {
		entry.spec.Name = newName
	}
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

// LookupByName findet den Web-Viewer dessen Name (case-insensitive,
// umlaut-tolerant, whitespace-tolerant) der Eingabe entspricht.
//
// Saison 13-02-FIX4-a-HOTFIX4: ersetzt LookupByUsername. Der
// Wohnungs-Name IST jetzt der Login; Mieter darf "Familie
// Mueller 2OG", "familie mueller 2og" oder "Dämgen" tippen und
// findet jedes Mal denselben Eintrag.
//
// Implementation: alle Web-Viewer rauslesen und in Go vergleichen.
// Bei <1000 Wohnungen pro Server vernachlaessigbar; SQLite hat
// kein Built-in fuer "deutsche Umlaute aufloesen und collapse
// whitespace", deshalb keine WHERE-Klausel.
func (m *Manager) LookupByName(ctx context.Context, name string) (*ViewerInfo, string, error) {
	target := NormalizeName(name)
	if target == "" {
		return nil, "", ErrViewerNotFound
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT mac, name, service_port, type, password_hash, password_set_at,
		        COALESCE(linked_ua_user_id, ''), esp_model, esp_fw_version, esp_token_hash
		   FROM viewers
		  WHERE type = 'web'`)
	if err != nil {
		return nil, "", fmt.Errorf("mockmanager: lookup name: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			info     ViewerInfo
			port     int64
			hash     sql.NullString
			setAt    sql.NullInt64
			espModel sql.NullString
			espFW    sql.NullString
			espHash  sql.NullString
		)
		if err := rows.Scan(&info.MAC, &info.Name, &port, &info.Type,
			&hash, &setAt, &info.LinkedUAUserID,
			&espModel, &espFW, &espHash); err != nil {
			return nil, "", fmt.Errorf("mockmanager: scan: %w", err)
		}
		if NormalizeName(info.Name) != target {
			continue
		}
		info.ServicePort = uint16(port)
		if hash.Valid {
			info.HasPassword = true
		}
		if setAt.Valid {
			t := time.UnixMilli(setAt.Int64)
			info.PasswordSetAt = &t
		}
		if espModel.Valid {
			info.ESPModel = espModel.String
		}
		if espFW.Valid {
			info.ESPFwVersion = espFW.String
		}
		if espHash.Valid && espHash.String != "" {
			info.HasESPToken = true
		}
		m.mu.Lock()
		if _, ok := m.viewers[info.MAC]; ok {
			info.Running = true
		}
		m.mu.Unlock()
		return &info, hashOrEmpty(hash), nil
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("mockmanager: rows: %w", err)
	}
	return nil, "", ErrViewerNotFound
}

// nameExistsLocked prueft ob ein Eintrag mit dem normalisierten
// Namen schon existiert. excludeMAC darf der MAC des aktuellen
// Subjects sein (fuer Rename-Pfade); leer = alles pruefen.
func (m *Manager) nameExistsLocked(ctx context.Context, target, excludeMAC string) (bool, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT mac, name FROM viewers`)
	if err != nil {
		return false, fmt.Errorf("mockmanager: name check: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var mac, name string
		if err := rows.Scan(&mac, &name); err != nil {
			return false, fmt.Errorf("mockmanager: name check scan: %w", err)
		}
		if mac == excludeMAC {
			continue
		}
		if NormalizeName(name) == target {
			return true, nil
		}
	}
	return false, rows.Err()
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
		info     ViewerInfo
		port     int64
		hash     sql.NullString
		setAt    sql.NullInt64
		espModel sql.NullString
		espFW    sql.NullString
		espHash  sql.NullString
	)
	err := m.db.QueryRowContext(ctx,
		`SELECT mac, name, service_port, type, password_hash, password_set_at,
		        COALESCE(linked_ua_user_id, ''), esp_model, esp_fw_version, esp_token_hash
		   FROM viewers WHERE mac = ?`, mac).
		Scan(&info.MAC, &info.Name, &port, &info.Type, &hash, &setAt,
			&info.LinkedUAUserID, &espModel, &espFW, &espHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrViewerNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("mockmanager: load info: %w", err)
	}
	info.ServicePort = uint16(port)
	if hash.Valid {
		info.HasPassword = true
	}
	if setAt.Valid {
		t := time.UnixMilli(setAt.Int64)
		info.PasswordSetAt = &t
	}
	if espModel.Valid {
		info.ESPModel = espModel.String
	}
	if espFW.Valid {
		info.ESPFwVersion = espFW.String
	}
	if espHash.Valid && espHash.String != "" {
		info.HasESPToken = true
	}
	return &info, nil
}

// ListViewers returns the snapshot of every persisted viewer
// (web + esp). Reads from the DB so esp-type rows are also
// surfaced; running flag comes from the in-memory map.
func (m *Manager) ListViewers(ctx context.Context) ([]ViewerInfo, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT mac, name, service_port, type, password_hash, password_set_at,
		        COALESCE(linked_ua_user_id, ''), esp_model, esp_fw_version, esp_token_hash
		   FROM viewers ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("mockmanager: list: %w", err)
	}
	defer rows.Close()
	out := make([]ViewerInfo, 0)
	for rows.Next() {
		var (
			info     ViewerInfo
			port     int64
			hash     sql.NullString
			setAt    sql.NullInt64
			espModel sql.NullString
			espFW    sql.NullString
			espHash  sql.NullString
		)
		if err := rows.Scan(&info.MAC, &info.Name, &port, &info.Type,
			&hash, &setAt, &info.LinkedUAUserID,
			&espModel, &espFW, &espHash); err != nil {
			return nil, fmt.Errorf("mockmanager: scan list: %w", err)
		}
		info.ServicePort = uint16(port)
		if hash.Valid {
			info.HasPassword = true
		}
		if setAt.Valid {
			t := time.UnixMilli(setAt.Int64)
			info.PasswordSetAt = &t
		}
		if espModel.Valid {
			info.ESPModel = espModel.String
		}
		if espFW.Valid {
			info.ESPFwVersion = espFW.String
		}
		if espHash.Valid && espHash.String != "" {
			info.HasESPToken = true
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
		   (mac, name, service_port, type, linked_ua_user_id,
		    esp_model, esp_fw_version, esp_token_hash,
		    created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		spec.MAC, spec.Name, int64(spec.ServicePort),
		spec.Type,
		nullable(spec.LinkedUAUserID),
		nullable(spec.ESPModel),
		nullable(spec.ESPFwVersion),
		nullable(spec.ESPTokenHash),
		now, now,
	)
	if err != nil {
		return fmt.Errorf("mockmanager: insert: %w", err)
	}
	return nil
}

// SetESPTokenHash speichert einen frisch generierten Token-Hash
// fuer einen adoptierten ESP-Viewer. Die alte token-hash-Zeile
// wird einfach ueberschrieben (Token-Rotation).
func (m *Manager) SetESPTokenHash(ctx context.Context, mac, hash string) error {
	now := m.opts.Now().UnixMilli()
	res, err := m.db.ExecContext(ctx,
		`UPDATE viewers SET esp_token_hash = ?, updated_at = ?
		 WHERE mac = ? AND type = 'esp'`,
		nullable(hash), now, mac)
	if err != nil {
		return fmt.Errorf("mockmanager: set esp token: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrViewerNotFound
	}
	return nil
}

// LookupESPTokenHash gibt den Token-Hash fuer einen adoptierten
// ESP-Viewer zurueck. Wird in FIX4-d von der Bearer-Auth-
// Middleware genutzt; in FIX4-c nur fuer die Status-Poll-Logik.
func (m *Manager) LookupESPTokenHash(ctx context.Context, mac string) (string, error) {
	var hash sql.NullString
	err := m.db.QueryRowContext(ctx,
		`SELECT esp_token_hash FROM viewers WHERE mac = ? AND type = 'esp'`,
		mac).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrViewerNotFound
	}
	if err != nil {
		return "", fmt.Errorf("mockmanager: lookup esp token: %w", err)
	}
	return hashOrEmpty(hash), nil
}

// SetLinkedUAUserID updates the optional UA-User-Verknuepfung.
// Empty userID clears the link. Web-Viewer-Edit-Pfad nutzt das.
func (m *Manager) SetLinkedUAUserID(ctx context.Context, mac, userID string) error {
	now := m.opts.Now().UnixMilli()
	res, err := m.db.ExecContext(ctx,
		`UPDATE viewers SET linked_ua_user_id = ?, updated_at = ? WHERE mac = ?`,
		nullable(userID), now, mac)
	if err != nil {
		return fmt.Errorf("mockmanager: set linked ua user: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrViewerNotFound
	}
	m.mu.Lock()
	if entry, ok := m.viewers[mac]; ok {
		entry.spec.LinkedUAUserID = userID
	}
	m.mu.Unlock()
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
