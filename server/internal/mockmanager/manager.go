// Package mockmanager owns the lifecycle of embedded mock viewers
// inside carvilon-server. Each viewer runs as a goroutine hosted by
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
	"strings"
	"sync"
	"time"

	"carvilon.local/mock"
	"carvilon.local/server/internal/auth/esptoken"
	"carvilon.local/server/internal/db"
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
	// RejectDoorbell publishes a /call_admin_result RPC to UDM so
	// the intercom stops ringing immediately. Saison 13-04.5-B.
	RejectDoorbell(intercomMAC string) error
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
	// PairedIntercomMAC is the UA-API intercom this viewer is
	// paired with for the standby "Tuer auf"-Knopf (saison-13-07).
	// Empty string = no pairing, standby button is inert. Stored
	// colon-form lowercase ("28:70:4e:31:e2:9c").
	PairedIntercomMAC string
	// StreamProfile is the go2rtc profile name this viewer's
	// /stream.mjpeg proxy resolves to (saison-14-01). Empty
	// string = convention fallback (TypeESP -> "intercom_esp",
	// TypeWeb -> "intercom_browser"). The admin /a/streams UI
	// manages the actual go2rtc YAML side; here we only remember
	// which profile each viewer requested.
	StreamProfile string
	// IdleViewMode chooses which idle UI the mieter browser
	// renders by default (saison-14-01b). "" or "screensaver"
	// render clock + date + weather; "livestream" puts the
	// MJPEG img directly. Tap toggles temporarily, reload goes
	// back to the persisted default.
	// Saison 14-XX added "screen_off" as a third valid value
	// (ESP backlight off; web viewers render it like screensaver).
	IdleViewMode string
	// AutoScreensaverSeconds enables the saison-14-03 auto-
	// fallback timer: if the mieter has switched to livestream /
	// settings / history mode and stays idle for this many
	// seconds, the runtime slides back to the screensaver.
	// nil = disabled. Only effective when IdleViewMode is
	// "screensaver" (otherwise there is nothing to fall back
	// to). Stored as INTEGER NULL in the DB.
	AutoScreensaverSeconds *int
}

// ViewerInfo is the public view of one running viewer for the
// admin UI.
type ViewerInfo struct {
	MAC                    string
	Name                   string
	ServicePort            uint16
	Type                   string
	HasPassword            bool
	PasswordSetAt          *time.Time
	LinkedUAUserID         string
	ESPModel               string
	ESPFwVersion           string
	HasESPToken            bool
	Running                bool
	PairedIntercomMAC      string // saison-13-07 standby pairing
	StreamProfile          string // saison-14-01 go2rtc profile override
	IdleViewMode           string // saison-14-01b "screensaver", "livestream" or "screen_off" (S14-XX); "" = default screensaver
	AutoScreensaverSeconds *int   // saison-14-03 auto-fallback timer; nil/0 = disabled
	// Saison 14-XX ESP-Settings (also accessible to web viewers
	// for the "language" choice; the two display-hardware fields
	// are only honoured by ESP firmware).
	BrightnessIdle    *int // 0..100; nil = use DefaultBrightnessIdle
	ScreenOffAfterSec *int // seconds; nil/0 = backlight stays on
	Language          string // "de"/"en"; "" = use DefaultLanguage
	// Saison 14-04-Phase2 history-capture toggle. nil = treat as
	// true (default); explicit false comes from the mieter
	// settings page. ResolveHistoryCaptureEnabled hides the NULL
	// detail from callers.
	HistoryCaptureEnabled *bool
}

// IdleViewMode constants. Storage tolerates NULL (= default
// screensaver); the helper below picks the right string for the
// template and the /esp/config JSON.
//
// Saison 14-XX added IdleViewModeScreenOff: ESP firmware turns
// the backlight off; web viewers render the slot identical to
// IdleViewModeScreensaver (the concept of "display off" does not
// apply to a browser tab).
const (
	IdleViewModeScreensaver = "screensaver"
	IdleViewModeLivestream  = "livestream"
	IdleViewModeScreenOff   = "screen_off"
)

// ResolveIdleViewMode picks the rendered mode for the calling
// viewer. NULL / empty falls back to the screensaver default.
// The value is returned as-is when valid so web-viewer JS can
// branch on it (and treat screen_off the same as screensaver
// while ESP firmware switches the backlight).
func (v *ViewerInfo) ResolveIdleViewMode() string {
	if v == nil {
		return IdleViewModeScreensaver
	}
	switch v.IdleViewMode {
	case IdleViewModeLivestream:
		return IdleViewModeLivestream
	case IdleViewModeScreenOff:
		return IdleViewModeScreenOff
	default:
		return IdleViewModeScreensaver
	}
}

// ResolveAutoScreensaverSeconds returns the persisted timer
// value, or 0 when the column is NULL (= feature off). The
// browser runtime treats 0 as "no auto-fallback"; the same is
// true when the viewer's idle_view_mode is "livestream"
// (handled client-side, not in the DB).
//
// Saison 14-03.
func (v *ViewerInfo) ResolveAutoScreensaverSeconds() int {
	if v == nil || v.AutoScreensaverSeconds == nil {
		return 0
	}
	return *v.AutoScreensaverSeconds
}

// Saison 14-XX ESP-Settings: Defaults + Allow-Lists.
//
// Defaults werden im Resolver-Layer angewandt damit ein nicht-
// gesetzter Wert (NULL in der DB) konsistent zwischen Web- und
// ESP-Pfad denselben Default sieht; die Migration legt KEINE
// DDL-Defaults an.
const (
	DefaultBrightnessIdle    = 70
	DefaultScreenOffAfterSec = 0 // 0 = Backlight bleibt an
	DefaultLanguage          = "de"
)

// ScreenOffAfterSecAllowed enumerates the persisted seconds-values
// for the ESP backlight-off timer. 0 disables the feature; the
// rest are common UI choices (30s, 1m, 5m, 10m, 30m). 0 stores
// SQL NULL via nullableInt, the rest go in as plain integers.
var ScreenOffAfterSecAllowed = []int{0, 30, 60, 300, 600, 1800}

// LanguageAllowed enumerates the UI-language values the ESP
// firmware understands today. Web viewers render German strings
// from the template regardless, so this column primarily steers
// the ESP firmware string tables.
var LanguageAllowed = []string{"de", "en"}

// ResolveBrightnessIdle returns the persisted idle-brightness or
// DefaultBrightnessIdle when the column is NULL. ESP-only
// concept; web viewers ignore it.
func (v *ViewerInfo) ResolveBrightnessIdle() int {
	if v == nil || v.BrightnessIdle == nil {
		return DefaultBrightnessIdle
	}
	return *v.BrightnessIdle
}

// ResolveScreenOffAfterSec returns the persisted backlight-off
// timer or DefaultScreenOffAfterSec (= 0, off) when NULL. ESP-
// only concept; web viewers ignore it.
func (v *ViewerInfo) ResolveScreenOffAfterSec() int {
	if v == nil || v.ScreenOffAfterSec == nil {
		return DefaultScreenOffAfterSec
	}
	return *v.ScreenOffAfterSec
}

// ResolveLanguage returns the persisted UI-language or
// DefaultLanguage when the column is empty. The same default
// applies to both web and ESP renderers.
func (v *ViewerInfo) ResolveLanguage() string {
	if v == nil || v.Language == "" {
		return DefaultLanguage
	}
	return v.Language
}

// ResolveHistoryCaptureEnabled returns the persisted toggle or
// true when the column is NULL (= legacy row that pre-dates
// Saison 14-04-Phase2). The mieter UI hides the whole history
// section when false; the server still writes door_events rows
// so the admin trail remains complete.
func (v *ViewerInfo) ResolveHistoryCaptureEnabled() bool {
	if v == nil || v.HistoryCaptureEnabled == nil {
		return true
	}
	return *v.HistoryCaptureEnabled
}

// ResolveStreamProfile picks the go2rtc stream profile name for
// this viewer. Order:
//
//  1. explicit StreamProfile if non-empty
//  2. TypeESP -> "intercom_esp"
//  3. TypeWeb -> "intercom_browser"
//  4. fallback "intercom_default" (defensive; should not happen
//     because Type is constrained to web/esp by the schema check)
//
// Convention is in lock-step with the go2rtc.yaml.example shipped
// with saison-14-01. Renaming a profile in go2rtc without updating
// the matching default here will leave new viewers pointed at a
// missing source until the admin picks one in /a/streams.
func (v *ViewerInfo) ResolveStreamProfile() string {
	if v == nil {
		return "intercom_default"
	}
	if v.StreamProfile != "" {
		return v.StreamProfile
	}
	switch v.Type {
	case TypeESP:
		return "intercom_esp"
	case TypeWeb:
		return "intercom_browser"
	}
	return "intercom_default"
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

// LoadFromDB reads every web- and esp-type row from viewers and
// starts a Mock-Goroutine per row. Called once at server boot.
//
// Saison 13-09: ESP-type rows are no longer skipped. Their
// goroutine handles the UDM-side adoption + Stage 1+4+5+6 stack
// the same way web-type rows do; the type distinction matters
// only at the auth surface. See AddViewer for the matching
// runtime spawn path.
func (m *Manager) LoadFromDB(ctx context.Context) error {
	rows, err := m.db.QueryContext(ctx,
		`SELECT mac, name, service_port, type,
		        COALESCE(linked_ua_user_id, '')
		   FROM viewers
		  WHERE type IN (?, ?)
		  ORDER BY mac`, TypeWeb, TypeESP)
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
	var web, esp int
	for _, spec := range specs {
		if err := m.startViewerLocked(spec); err != nil {
			m.log.Error("start viewer failed during load",
				"mac", spec.MAC, "type", spec.Type, "err", err)
			continue
		}
		if spec.Type == TypeESP {
			esp++
		} else {
			web++
		}
	}
	m.log.Info("loaded viewers", "web", web, "esp", esp)
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
	// In-Memory hat seit S13-09 die laufenden web- UND esp-type-
	// Viewer. Vor LoadFromDB persistierte Reihen muessen trotzdem
	// direkt aus der DB geprueft werden (weil der Map vor dem
	// Boot-Reload leer ist).
	if exists, err := m.nameExistsLocked(ctx, specKey, spec.MAC); err != nil {
		return err
	} else if exists {
		return ErrNameInUse
	}

	if err := m.insertViewerLocked(ctx, spec); err != nil {
		return err
	}

	// Saison 13-09: spawn the mock-goroutine for both web- and
	// esp-type viewers. The type distinction matters for the
	// browser-vs-bearer auth surface (web has cookie sessions
	// from /webviewer, esp has bearer tokens at /esp/), but on
	// the UDM-facing side both run the same Stage 1+4+5+6 stack
	// so that the ESP-Hardware can subscribe to /esp/events and
	// receive doorbell.ring frames the same way the web-Mieter
	// does on /webviewer/events.
	if spec.Type == TypeWeb || spec.Type == TypeESP {
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

// LookupForReject returns the running Viewer instance for the given
// MAC. Test-only seam so test code can verify what RejectDoorbell
// has been asked to publish without going through the manager's
// own publish path. Production callers must use RejectDoorbellOnMock.
func (m *Manager) LookupForReject(mac string) (Viewer, error) {
	m.mu.Lock()
	entry, ok := m.viewers[mac]
	m.mu.Unlock()
	if !ok {
		return nil, ErrViewerNotFound
	}
	return entry.viewer, nil
}

// RejectDoorbellOnMock looks up the running viewer by mock-MAC and
// asks it to publish a /call_admin_result RPC that ends the active
// doorbell call from intercomMAC. Returns ErrViewerNotFound if the
// MAC is not currently running; callers are expected to log + drop
// (the lifecycle row was already updated in doorbellcalls before
// this gets called).
//
// Saison 13-04.5-B: lets the mieter "Ignorieren" / "Anruf beenden"
// endpoints silence the intercom hardware immediately instead of
// waiting for the 30-second UDM-side timeout.
func (m *Manager) RejectDoorbellOnMock(mac, intercomMAC string) error {
	m.mu.Lock()
	entry, ok := m.viewers[mac]
	m.mu.Unlock()
	if !ok {
		return ErrViewerNotFound
	}
	return entry.viewer.RejectDoorbell(intercomMAC)
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
		        COALESCE(linked_ua_user_id, ''), esp_model, esp_fw_version, esp_token_hash,
		        COALESCE(paired_intercom_mac, ''), COALESCE(stream_profile, ''),
		        COALESCE(idle_view_mode, ''),
		        auto_screensaver_seconds,
		        brightness_idle, screen_off_after_sec,
		        COALESCE(language, ''),
		        history_capture_enabled
		   FROM viewers
		  WHERE type = 'web'`)
	if err != nil {
		return nil, "", fmt.Errorf("mockmanager: lookup name: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			info       ViewerInfo
			port       int64
			hash       sql.NullString
			setAt      sql.NullInt64
			espModel   sql.NullString
			espFW      sql.NullString
			espHash    sql.NullString
			autoSec    sql.NullInt64
			brightness sql.NullInt64
			screenOff  sql.NullInt64
			capture    sql.NullInt64
		)
		if err := rows.Scan(&info.MAC, &info.Name, &port, &info.Type,
			&hash, &setAt, &info.LinkedUAUserID,
			&espModel, &espFW, &espHash, &info.PairedIntercomMAC,
			&info.StreamProfile, &info.IdleViewMode, &autoSec,
			&brightness, &screenOff, &info.Language, &capture); err != nil {
			return nil, "", fmt.Errorf("mockmanager: scan: %w", err)
		}
		if autoSec.Valid {
			v := int(autoSec.Int64)
			info.AutoScreensaverSeconds = &v
		}
		if brightness.Valid {
			v := int(brightness.Int64)
			info.BrightnessIdle = &v
		}
		if screenOff.Valid {
			v := int(screenOff.Int64)
			info.ScreenOffAfterSec = &v
		}
		if capture.Valid {
			v := capture.Int64 != 0
			info.HistoryCaptureEnabled = &v
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
		info       ViewerInfo
		port       int64
		hash       sql.NullString
		setAt      sql.NullInt64
		espModel   sql.NullString
		espFW      sql.NullString
		espHash    sql.NullString
		autoSec    sql.NullInt64
		brightness sql.NullInt64
		screenOff  sql.NullInt64
		capture    sql.NullInt64
	)
	err := m.db.QueryRowContext(ctx,
		`SELECT mac, name, service_port, type, password_hash, password_set_at,
		        COALESCE(linked_ua_user_id, ''), esp_model, esp_fw_version, esp_token_hash,
		        COALESCE(paired_intercom_mac, ''), COALESCE(stream_profile, ''),
		        COALESCE(idle_view_mode, ''),
		        auto_screensaver_seconds,
		        brightness_idle, screen_off_after_sec,
		        COALESCE(language, ''),
		        history_capture_enabled
		   FROM viewers WHERE mac = ?`, mac).
		Scan(&info.MAC, &info.Name, &port, &info.Type, &hash, &setAt,
			&info.LinkedUAUserID, &espModel, &espFW, &espHash, &info.PairedIntercomMAC,
			&info.StreamProfile, &info.IdleViewMode, &autoSec,
			&brightness, &screenOff, &info.Language, &capture)
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
	if autoSec.Valid {
		v := int(autoSec.Int64)
		info.AutoScreensaverSeconds = &v
	}
	if brightness.Valid {
		v := int(brightness.Int64)
		info.BrightnessIdle = &v
	}
	if screenOff.Valid {
		v := int(screenOff.Int64)
		info.ScreenOffAfterSec = &v
	}
	if capture.Valid {
		v := capture.Int64 != 0
		info.HistoryCaptureEnabled = &v
	}
	return &info, nil
}

// ListViewers returns the snapshot of every persisted viewer
// (web + esp). Reads from the DB so esp-type rows are also
// surfaced; running flag comes from the in-memory map.
func (m *Manager) ListViewers(ctx context.Context) ([]ViewerInfo, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT mac, name, service_port, type, password_hash, password_set_at,
		        COALESCE(linked_ua_user_id, ''), esp_model, esp_fw_version, esp_token_hash,
		        COALESCE(paired_intercom_mac, ''), COALESCE(stream_profile, ''),
		        COALESCE(idle_view_mode, ''),
		        auto_screensaver_seconds,
		        brightness_idle, screen_off_after_sec,
		        COALESCE(language, ''),
		        history_capture_enabled
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
			hash       sql.NullString
			setAt      sql.NullInt64
			espModel   sql.NullString
			espFW      sql.NullString
			espHash    sql.NullString
			autoSec    sql.NullInt64
			brightness sql.NullInt64
			screenOff  sql.NullInt64
			capture    sql.NullInt64
		)
		if err := rows.Scan(&info.MAC, &info.Name, &port, &info.Type,
			&hash, &setAt, &info.LinkedUAUserID,
			&espModel, &espFW, &espHash, &info.PairedIntercomMAC,
			&info.StreamProfile, &info.IdleViewMode, &autoSec,
			&brightness, &screenOff, &info.Language, &capture); err != nil {
			return nil, fmt.Errorf("mockmanager: scan list: %w", err)
		}
		if autoSec.Valid {
			v := int(autoSec.Int64)
			info.AutoScreensaverSeconds = &v
		}
		if brightness.Valid {
			v := int(brightness.Int64)
			info.BrightnessIdle = &v
		}
		if screenOff.Valid {
			v := int(screenOff.Int64)
			info.ScreenOffAfterSec = &v
		}
		if capture.Valid {
			v := capture.Int64 != 0
			info.HistoryCaptureEnabled = &v
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
		    paired_intercom_mac, stream_profile, idle_view_mode,
		    auto_screensaver_seconds,
		    created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		spec.MAC, spec.Name, int64(spec.ServicePort),
		spec.Type,
		nullable(spec.LinkedUAUserID),
		nullable(spec.ESPModel),
		nullable(spec.ESPFwVersion),
		nullable(spec.ESPTokenHash),
		strings.ToLower(strings.TrimSpace(spec.PairedIntercomMAC)),
		nullable(strings.TrimSpace(spec.StreamProfile)),
		nullable(strings.TrimSpace(spec.IdleViewMode)),
		nullableInt(spec.AutoScreensaverSeconds),
		now, now,
	)
	if err != nil {
		return fmt.Errorf("mockmanager: insert: %w", err)
	}
	return nil
}

// SetPairedIntercomMAC updates a viewer's paired intercom (the
// standby "Tuer auf"-Knopf source). Empty string clears the
// pairing - standby button becomes inert until set again.
//
// Saison 13-07. Pair value is normalised to lowercase + trimmed
// before write so future LookupDoorForIntercom calls match
// regardless of how the admin typed the MAC.
func (m *Manager) SetPairedIntercomMAC(ctx context.Context, mac, intercomMAC string) error {
	now := m.opts.Now().UnixMilli()
	res, err := m.db.ExecContext(ctx,
		`UPDATE viewers SET paired_intercom_mac = ?, updated_at = ? WHERE mac = ?`,
		strings.ToLower(strings.TrimSpace(intercomMAC)), now, mac)
	if err != nil {
		return fmt.Errorf("mockmanager: set paired intercom: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrViewerNotFound
	}
	m.mu.Lock()
	if entry, ok := m.viewers[mac]; ok {
		entry.spec.PairedIntercomMAC = strings.ToLower(strings.TrimSpace(intercomMAC))
	}
	m.mu.Unlock()
	return nil
}

// SetStreamProfile updates a viewer's go2rtc stream profile name.
// Empty string clears the override - the viewer falls back to the
// type-based convention (see ResolveStreamProfile). The value is
// stored trimmed; no further validation here, because go2rtc may
// hold profiles we are not aware of (and the admin UI already
// limits the input to the live profile list).
//
// Saison 14-01.
func (m *Manager) SetStreamProfile(ctx context.Context, mac, profile string) error {
	now := m.opts.Now().UnixMilli()
	trimmed := strings.TrimSpace(profile)
	res, err := m.db.ExecContext(ctx,
		`UPDATE viewers SET stream_profile = ?, updated_at = ? WHERE mac = ?`,
		nullable(trimmed), now, mac)
	if err != nil {
		return fmt.Errorf("mockmanager: set stream profile: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrViewerNotFound
	}
	m.mu.Lock()
	if entry, ok := m.viewers[mac]; ok {
		entry.spec.StreamProfile = trimmed
	}
	m.mu.Unlock()
	return nil
}

// SetIdleViewMode updates a viewer's idle-view-mode preference.
// Empty string clears it (next render falls back to "screensaver").
// Any non-empty value other than IdleViewModeScreensaver /
// IdleViewModeLivestream / IdleViewModeScreenOff is rejected so
// we never persist garbage that a future template lookup would
// not recognise.
//
// Saison 14-01b; saison 14-XX added "screen_off" for ESP backlight.
func (m *Manager) SetIdleViewMode(ctx context.Context, mac, mode string) error {
	trimmed := strings.TrimSpace(mode)
	switch trimmed {
	case "", IdleViewModeScreensaver, IdleViewModeLivestream, IdleViewModeScreenOff:
	default:
		return fmt.Errorf("mockmanager: idle view mode %q must be %q, %q or %q",
			trimmed, IdleViewModeScreensaver, IdleViewModeLivestream, IdleViewModeScreenOff)
	}
	now := m.opts.Now().UnixMilli()
	res, err := m.db.ExecContext(ctx,
		`UPDATE viewers SET idle_view_mode = ?, updated_at = ? WHERE mac = ?`,
		nullable(trimmed), now, mac)
	if err != nil {
		return fmt.Errorf("mockmanager: set idle view mode: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrViewerNotFound
	}
	m.mu.Lock()
	if entry, ok := m.viewers[mac]; ok {
		entry.spec.IdleViewMode = trimmed
	}
	m.mu.Unlock()
	return nil
}

// SetBrightnessIdle persistiert die ESP-Idle-Helligkeit (Range
// 0..100). Werte ausserhalb der Range werden zurueckgewiesen.
// Saison 14-XX.
func (m *Manager) SetBrightnessIdle(ctx context.Context, mac string, value int) error {
	if value < 0 || value > 100 {
		return fmt.Errorf("mockmanager: brightness_idle %d must be in 0..100", value)
	}
	now := m.opts.Now().UnixMilli()
	res, err := m.db.ExecContext(ctx,
		`UPDATE viewers SET brightness_idle = ?, updated_at = ? WHERE mac = ?`,
		int64(value), now, mac)
	if err != nil {
		return fmt.Errorf("mockmanager: set brightness idle: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrViewerNotFound
	}
	m.mu.Lock()
	if _, ok := m.viewers[mac]; ok {
		// ViewerSpec haelt brightness_idle nicht; aktualisierung
		// lazy beim naechsten loadInfo. Lock-Release nur fuer den
		// existence-Check noetig (keine cache-Mutation).
	}
	m.mu.Unlock()
	return nil
}

// SetScreenOffAfterSec persistiert den ESP-Backlight-Off-Timer.
// Werte ausserhalb ScreenOffAfterSecAllowed werden zurueckgewiesen.
// 0 disabled das Feature und speichert SQL NULL.
// Saison 14-XX.
func (m *Manager) SetScreenOffAfterSec(ctx context.Context, mac string, value int) error {
	allowed := false
	for _, v := range ScreenOffAfterSecAllowed {
		if v == value {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("mockmanager: screen_off_after_sec %d not in %v",
			value, ScreenOffAfterSecAllowed)
	}
	var stored any
	if value > 0 {
		stored = int64(value)
	}
	now := m.opts.Now().UnixMilli()
	res, err := m.db.ExecContext(ctx,
		`UPDATE viewers SET screen_off_after_sec = ?, updated_at = ? WHERE mac = ?`,
		stored, now, mac)
	if err != nil {
		return fmt.Errorf("mockmanager: set screen off after sec: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrViewerNotFound
	}
	return nil
}

// SetHistoryCaptureEnabled persistiert den Mieter-Datenschutz-
// Toggle. true = Mieter sieht den Verlauf wieder; false = die
// Mieter-API liefert eine leere Liste (mit capture_enabled-Flag).
// Admin-Pfade sind unbeeinflusst - der Toggle aendert nur was
// die Mieter-UI rendert.
//
// Saison 14-04-Phase2.
func (m *Manager) SetHistoryCaptureEnabled(ctx context.Context, mac string, enabled bool) error {
	var stored int64
	if enabled {
		stored = 1
	}
	now := m.opts.Now().UnixMilli()
	res, err := m.db.ExecContext(ctx,
		`UPDATE viewers SET history_capture_enabled = ?, updated_at = ? WHERE mac = ?`,
		stored, now, mac)
	if err != nil {
		return fmt.Errorf("mockmanager: set history capture: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrViewerNotFound
	}
	return nil
}

// SetLanguage persistiert die UI-Sprache. Werte ausserhalb der
// Allow-Liste werden zurueckgewiesen. Empty erlaubt = "auf
// Default zuruecksetzen" (NULL in der DB).
// Saison 14-XX.
func (m *Manager) SetLanguage(ctx context.Context, mac, value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed != "" {
		allowed := false
		for _, v := range LanguageAllowed {
			if v == trimmed {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("mockmanager: language %q not in %v",
				trimmed, LanguageAllowed)
		}
	}
	now := m.opts.Now().UnixMilli()
	res, err := m.db.ExecContext(ctx,
		`UPDATE viewers SET language = ?, updated_at = ? WHERE mac = ?`,
		nullable(trimmed), now, mac)
	if err != nil {
		return fmt.Errorf("mockmanager: set language: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrViewerNotFound
	}
	return nil
}

// AutoScreensaverSecondsAllowed is the closed set of values the
// saison-14-03 inline-settings form may persist. 0 means "off"
// and is stored as SQL NULL; the others are seconds.
var AutoScreensaverSecondsAllowed = []int{0, 30, 60, 300, 600}

// SetAutoScreensaverSeconds updates the auto-fallback timer for
// the given viewer. Pass 0 to disable the timer (the column
// becomes NULL); pass any of {30, 60, 300, 600} to enable it.
// Other values are rejected up-front so a future regression on
// the POST handler does not let arbitrary integers reach the
// browser runtime.
//
// Saison 14-03.
func (m *Manager) SetAutoScreensaverSeconds(ctx context.Context, mac string, seconds int) error {
	allowed := false
	for _, v := range AutoScreensaverSecondsAllowed {
		if v == seconds {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("mockmanager: auto_screensaver_seconds %d not in %v",
			seconds, AutoScreensaverSecondsAllowed)
	}
	var stored any
	if seconds > 0 {
		stored = int64(seconds)
	}
	now := m.opts.Now().UnixMilli()
	res, err := m.db.ExecContext(ctx,
		`UPDATE viewers SET auto_screensaver_seconds = ?, updated_at = ? WHERE mac = ?`,
		stored, now, mac)
	if err != nil {
		return fmt.Errorf("mockmanager: set auto screensaver: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrViewerNotFound
	}
	m.mu.Lock()
	if entry, ok := m.viewers[mac]; ok {
		if seconds > 0 {
			v := seconds
			entry.spec.AutoScreensaverSeconds = &v
		} else {
			entry.spec.AutoScreensaverSeconds = nil
		}
	}
	m.mu.Unlock()
	return nil
}

// SiblingESPMACs liefert alle ESP-Viewer-MACs die an demselben
// UA-User haengen wie der uebergebene MAC, ausser dem MAC selbst.
// Wird vom /esp/answer-Pfad genutzt um "answered elsewhere"-
// Cancel-Events an die anderen Geraete des Mieters zu pushen.
// Wenn der Viewer keine linked_ua_user_id hat, gibt es per
// Definition keine Siblings (leere Liste, kein Fehler).
func (m *Manager) SiblingESPMACs(ctx context.Context, mac string) ([]string, error) {
	var linked sql.NullString
	err := m.db.QueryRowContext(ctx,
		`SELECT linked_ua_user_id FROM viewers WHERE mac = ?`, mac).Scan(&linked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrViewerNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("mockmanager: sibling lookup self: %w", err)
	}
	if !linked.Valid || linked.String == "" {
		return nil, nil
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT mac FROM viewers
		  WHERE type = 'esp' AND linked_ua_user_id = ? AND mac <> ?`,
		linked.String, mac)
	if err != nil {
		return nil, fmt.Errorf("mockmanager: sibling query: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var sibling string
		if err := rows.Scan(&sibling); err != nil {
			return nil, fmt.Errorf("mockmanager: sibling scan: %w", err)
		}
		out = append(out, sibling)
	}
	return out, rows.Err()
}

// TouchESPSeen aktualisiert nur updated_at fuer einen ESP-Viewer.
// Wird vom /esp/heartbeat-Fallback und vom /esp/state-Endpoint
// genutzt, damit das Admin-Dashboard ein "zuletzt gesehen"
// rendern kann ohne dass jeder Poll andere Spalten anfasst.
func (m *Manager) TouchESPSeen(ctx context.Context, mac string) error {
	now := m.opts.Now().UnixMilli()
	res, err := m.db.ExecContext(ctx,
		`UPDATE viewers SET updated_at = ? WHERE mac = ? AND type = 'esp'`,
		now, mac)
	if err != nil {
		return fmt.Errorf("mockmanager: touch esp: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrViewerNotFound
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

// LookupESPMACByToken vergleicht einen vom ESP praesentierten
// Klartext-Bearer-Token gegen alle adoptierten ESP-Viewer und
// liefert die MAC des passenden Geraets. Verify nutzt
// crypto/subtle.ConstantTimeCompare. Bei <100 ESP-Viewern pro
// Server (realistisch fuer eine Wohnanlage) ist die linear-
// scan-Strategie billig genug; in einer spaeteren Saison kann
// das auf indizierten Hash-Lookup umgestellt werden, sobald
// Multi-Tenant-Server gewachsen sind.
func (m *Manager) LookupESPMACByToken(ctx context.Context, presented string) (string, error) {
	if presented == "" {
		return "", ErrViewerNotFound
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT mac, esp_token_hash FROM viewers
		  WHERE type = 'esp' AND esp_token_hash IS NOT NULL`)
	if err != nil {
		return "", fmt.Errorf("mockmanager: lookup esp by token: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var mac string
		var hash sql.NullString
		if err := rows.Scan(&mac, &hash); err != nil {
			return "", fmt.Errorf("mockmanager: scan esp token: %w", err)
		}
		if !hash.Valid || hash.String == "" {
			continue
		}
		if esptoken.Verify(presented, hash.String) {
			return mac, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("mockmanager: rows: %w", err)
	}
	return "", ErrViewerNotFound
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

// nullableInt mirrors nullable for pointer-to-int spec fields:
// nil and 0 both become SQL NULL, anything else stores the int.
// Saison 14-03 uses this for AutoScreensaverSeconds where 0 is
// the same as "feature off".
func nullableInt(p *int) any {
	if p == nil || *p == 0 {
		return nil
	}
	return int64(*p)
}

func hashOrEmpty(h sql.NullString) string {
	if h.Valid {
		return h.String
	}
	return ""
}
