// Package viewermanager owns the lifecycle of embedded mock viewers
// inside carvilon-server. Each viewer runs as a goroutine hosted by
// the server process; the manager loads persisted specs from the
// viewers table on boot, starts the goroutines, multiplexes their
// event channels, and handles admin-driven add / remove operations.
//
// The persistence table is `viewers`
// (was mock_viewers) and rows can be of type 'web' or 'esp'. The
// manager only spawns goroutines for type 'web'; ESP viewers are
// authenticated separately and run on real hardware.
//
// The manager exposes a Viewer interface and a ViewerFactory so
// tests can inject a fake viewer instead of spinning up the real
// mock stack against a non-existent UDM.
//
// Saison 15-03 rename: the package directory + name went from
// `mockmanager` to `viewermanager` (the `mock` package itself
// keeps its name - "mock" is the honest description of the
// synthetic device emulation).
package viewermanager

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
	"carvilon.local/server/internal/normalize"
	"carvilon.local/server/internal/viewerstore"
)

// Channel buffer for the multiplexed event streams. The manager
// drops on overflow, like the per-viewer channels.
const (
	multiplexEventBuffer  = 64
	multiplexCancelBuffer = 64
)

// Viewer-Type-Konstanten (Spalte viewers.type).
//
// TypeWeb covers the browser tenant; TypeESP covers the ESP32-P4
// hardware-display tenant; TypeAndroid covers the native Android
// app (Saison 16 Etappe 1). Web, ESP and Android each spawn an
// embedded UDM-mock goroutine; the UDM adopts every row as a
// regular UA-Int-Viewer (S13-09 pattern). From the controller's
// perspective the three types are indistinguishable - the type
// column is a platform-side discriminator for the auth surface
// (cookie session for web on /webviewer/, bearer token for esp
// on /esp/, bearer token for android on /webviewer/) and the
// admin UI tab the row lives under. Doorbell RPCs reach all
// three the same way; the FCM-push pendant for Android in
// Etappe 2 is an ADDITIONAL push leg on top of the mock
// goroutine, not its replacement.
const (
	TypeWeb     = "web"
	TypeESP     = "esp"
	TypeAndroid = "android"
)

// Sentinel errors. Callers check via errors.Is.
var (
	ErrMACInUse       = errors.New("viewermanager: mac already registered")
	ErrPortInUse      = errors.New("viewermanager: service_port already registered")
	ErrViewerNotFound = errors.New("viewermanager: viewer not found")
	ErrNameInUse      = errors.New("viewermanager: viewer name already in use")
)

// Viewer is the subset of mock.Viewer that Manager needs. Defined
// as an interface so tests can inject a fake.
type Viewer interface {
	Run(ctx context.Context) error
	Events() <-chan mock.DoorbellEvent
	Cancels() <-chan mock.DoorbellCancelEvent
	MAC() string
	// RejectDoorbell publishes a /call_admin_result RPC to UDM so
	// the intercom stops ringing immediately.
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
// There is no separate username column; the Wohnungs-Name is the
// login itself. ESPModel / ESPFwVersion / DeviceTokenHash are only
// honoured for Type='esp'; on TypeWeb they stay empty.
type ViewerSpec struct {
	MAC            string
	Name           string
	ServicePort    uint16
	Type           string // TypeWeb / TypeESP. Empty defaults to TypeWeb.
	LinkedUAUserID string // optional UA-Access-User link
	ESPModel       string
	ESPFwVersion   string
	DeviceTokenHash   string
	// PairedIntercomMAC is the UA-API intercom this viewer is
	// paired with for the standby "Tuer auf" button. Empty string
	// = no pairing, standby button is inert. Stored colon-form
	// lowercase ("28:70:4e:31:e2:9c").
	PairedIntercomMAC string
	// StreamProfile is the go2rtc profile name this viewer's
	// /stream.mjpeg proxy resolves to. Empty string = convention
	// fallback (TypeESP -> "intercom_esp", TypeWeb ->
	// "intercom_browser"). The admin /a/streams UI manages the
	// actual go2rtc YAML side; here we only remember which
	// profile each viewer requested.
	StreamProfile string
	// IdleViewMode chooses which idle UI the mieter browser
	// renders by default. "" or "screensaver" render clock +
	// date + weather; "livestream" puts the MJPEG img directly;
	// "screen_off" turns the ESP backlight off (web viewers
	// render it identical to "screensaver"). Tap toggles
	// temporarily, reload returns to the persisted default.
	IdleViewMode string
	// AutoScreensaverSeconds enables the auto-fallback timer: if
	// the mieter has switched to livestream / settings / history
	// mode and stays idle for this many seconds, the runtime
	// slides back to the screensaver. nil = disabled. Only
	// effective when IdleViewMode is "screensaver"; otherwise
	// there is nothing to fall back to. Stored as INTEGER NULL
	// in the DB.
	AutoScreensaverSeconds *int
	// DeviceLabel is the operator-facing device name shown in
	// the admin list, used to tell multiple devices of the same
	// flat apart ("Papas Handy", "Mamas Handy"). Saison 16
	// Etappe 1 introduces this for the Android viewer where
	// many devices can share the same UA-user / paired-intercom
	// binding. Empty string = no label.
	DeviceLabel string
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
	HasDeviceToken            bool
	Running                bool
	PairedIntercomMAC      string // standby intercom pairing
	StreamProfile          string // go2rtc profile override
	IdleViewMode           string // "screensaver", "livestream" or "screen_off"; "" = default screensaver
	AutoScreensaverSeconds *int   // auto-fallback timer; nil/0 = disabled
	// ESP settings (also accessible to web viewers for the
	// "language" choice; the two display-hardware fields are only
	// honoured by ESP firmware).
	BrightnessIdle    *int // 0..100; nil = use DefaultBrightnessIdle
	ScreenOffAfterSec *int // seconds; nil/0 = backlight stays on
	Language          string // "de"/"en"; "" = use DefaultLanguage
	// History-capture toggle. nil = treat as true (default);
	// explicit false comes from the mieter settings page.
	// ResolveHistoryCaptureEnabled hides the NULL detail from
	// callers.
	HistoryCaptureEnabled *bool
	// Clock layout. "" = use Default (vertical, Pixel-Style);
	// explicit "horizontal" or "vertical" come from mieter /
	// admin / esp settings POSTs.
	ClockLayout string
	// PathMode is the per-viewer transport-path override (WEG-Schalter,
	// Saison 19-39): "auto" (default) / "local" / "cloud". Read-only
	// here; the app honours it when choosing the edge-vs-cloud endpoint.
	PathMode string
}

// IdleViewMode constants. Storage tolerates NULL (= default
// screensaver); the helper below picks the right string for the
// template and the /esp/config JSON.
//
// IdleViewModeScreenOff turns the ESP backlight off; web viewers
// render the slot identical to IdleViewModeScreensaver (the
// concept of "display off" does not apply to a browser tab).
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
func (v *ViewerInfo) ResolveAutoScreensaverSeconds() int {
	if v == nil || v.AutoScreensaverSeconds == nil {
		return 0
	}
	return *v.AutoScreensaverSeconds
}

// ESP-settings defaults + allow-lists.
//
// Defaults are applied in the resolver layer so an unset value
// (NULL in the DB) sees the same default on the web and ESP
// paths; the migration deliberately does NOT add DDL defaults.
const (
	DefaultBrightnessIdle    = 70
	DefaultScreenOffAfterSec = 0 // 0 = backlight stays on
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

// Clock-layout preference for the screensaver clock:
//   "vertical"   = Pixel-Style with HH stacked above MM
//   "horizontal" = classic HH:MM with colon
const (
	ClockLayoutVertical   = "vertical"
	ClockLayoutHorizontal = "horizontal"
	DefaultClockLayout    = ClockLayoutVertical
)

// ClockLayoutAllowed is the strict allow-list for every settings
// setter (web + ESP + admin).
var ClockLayoutAllowed = []string{ClockLayoutHorizontal, ClockLayoutVertical}

// PathMode values (Saison 19-39, the WEG-Schalter). "auto" = today's
// LAN-first/cloud-fallback behaviour; "local" = edge only; "cloud" =
// cloud only. carvilon only stores + serves the flag; the app honours it.
const (
	PathModeAuto  = "auto"
	PathModeLocal = "local"
	PathModeCloud = "cloud"
)

// PathModeAllowed is the strict allow-list for SetPathMode.
var PathModeAllowed = []string{PathModeAuto, PathModeLocal, PathModeCloud}

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
// true when the column is NULL (= legacy row that pre-dates the
// toggle). The mieter UI hides the whole history section when
// false; the server still writes door_events rows so the admin
// trail remains complete.
func (v *ViewerInfo) ResolveHistoryCaptureEnabled() bool {
	if v == nil || v.HistoryCaptureEnabled == nil {
		return true
	}
	return *v.HistoryCaptureEnabled
}

// ResolveClockLayout returns "horizontal" or "vertical". A
// NULL / empty column falls back to DefaultClockLayout
// (vertical, Pixel-Style).
func (v *ViewerInfo) ResolveClockLayout() string {
	if v == nil {
		return DefaultClockLayout
	}
	switch v.ClockLayout {
	case ClockLayoutHorizontal:
		return ClockLayoutHorizontal
	case ClockLayoutVertical:
		return ClockLayoutVertical
	default:
		return DefaultClockLayout
	}
}

// ResolvePathMode returns the viewer's transport-path override
// (WEG-Schalter, Saison 19-39). NULL/empty/unknown falls back to
// PathModeAuto (today's behaviour). carvilon only reports it; the app
// honours it when choosing the edge-vs-cloud endpoint.
func (v *ViewerInfo) ResolvePathMode() string {
	if v == nil {
		return PathModeAuto
	}
	switch v.PathMode {
	case PathModeLocal:
		return PathModeLocal
	case PathModeCloud:
		return PathModeCloud
	default:
		return PathModeAuto
	}
}

// ResolveStreamProfile picks the stream profile name for this
// viewer. Order:
//
//  1. explicit StreamProfile if non-empty
//  2. TypeESP     -> "intercom_esp"     (MJPEG, /esp/stream.mjpeg)
//  3. TypeWeb     -> "intercom_web"     (H.264 passthrough WebRTC, /offer)
//  4. TypeAndroid -> "intercom_android" (H.264 passthrough WebRTC, /offer)
//  5. fallback "intercom_default" (defensive; should not happen
//     because Type is constrained by the schema check)
//
// Convention is in lock-step with the profiles the streaming
// server ships:
//   - Web viewers go via WebRTC; the /offer endpoint only accepts
//     h264_passthrough sources, and the canonical such profile is
//     intercom_web. mjpeg_bal would 400 here because it is MJPEG.
//   - ESP devices keep the MJPEG passthrough; intercom_esp is the
//     low-bandwidth MJPEG profile sized for the ESP32-P4 display.
//   - Android (Saison 16 Etappe 1) shares the WebRTC stack with
//     web but pulls a distinct profile so the operator can tune
//     bitrate / resolution for mobile-data clients independently.
//     The stream-server holds intercom_android as a persistent
//     clone of intercom_web (codec h264_passthrough, same camera_id
//     and width/height/fps=0 passthrough); confirmed live by the
//     stream-chat before this commit landed.
//
// Renaming a profile on the backend without updating the matching
// default here will leave new viewers pointed at a missing source
// until the admin picks one in /a/streams.
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
		return "intercom_web"
	case TypeAndroid:
		return "intercom_android"
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
		log:      log.With("component", "viewermanager"),
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
// starts a mock goroutine per row. Called once at server boot.
//
// ESP-type rows are spawned with the same UDM-side stack as
// web-type rows (Stage 1+4+5+6); the type distinction only
// matters at the auth surface. See AddViewer for the matching
// runtime spawn path.
func (m *Manager) LoadFromDB(ctx context.Context) error {
	rows, err := m.db.QueryContext(ctx,
		`SELECT mac, name, service_port, type,
		        COALESCE(linked_ua_user_id, '')
		   FROM viewers
		  WHERE type IN (?, ?, ?)
		  ORDER BY mac`, TypeWeb, TypeESP, TypeAndroid)
	if err != nil {
		return fmt.Errorf("viewermanager: load: %w", err)
	}
	defer rows.Close()

	specs := make([]ViewerSpec, 0)
	for rows.Next() {
		var spec ViewerSpec
		var port int64
		if err := rows.Scan(&spec.MAC, &spec.Name, &port, &spec.Type, &spec.LinkedUAUserID); err != nil {
			return fmt.Errorf("viewermanager: scan: %w", err)
		}
		spec.ServicePort = uint16(port)
		specs = append(specs, spec)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("viewermanager: rows: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	var web, esp, android int
	for _, spec := range specs {
		// Every type (web, esp, android) gets a mock-goroutine
		// so the UDM adopts the row as a UA-Int-Viewer (S13-09
		// pattern). The auth-surface split happens at the HTTP
		// layer, not here.
		if err := m.startViewerLocked(spec); err != nil {
			m.log.Error("start viewer failed during load",
				"mac", spec.MAC, "type", spec.Type, "err", err)
			continue
		}
		switch spec.Type {
		case TypeESP:
			esp++
		case TypeWeb:
			web++
		case TypeAndroid:
			android++
		}
	}
	m.log.Info("loaded viewers", "web", web, "esp", esp, "android", android)
	return nil
}

// AddViewer registers a new viewer: persists it to viewers then
// spawns its goroutine. Returns ErrMACInUse, ErrPortInUse or
// ErrNameInUse on collision with an existing row.
//
// Name uniqueness is normalised (case + umlauts + whitespace),
// so two entries "Familie Mueller" and "FAMILIE MUELLER" are
// caught as duplicates.
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
	specKey := normalize.ViewerName(spec.Name)
	for _, e := range m.viewers {
		if e.spec.ServicePort == spec.ServicePort {
			return ErrPortInUse
		}
		if normalize.ViewerName(e.spec.Name) == specKey {
			return ErrNameInUse
		}
	}
	// In-memory holds every running viewer (web/esp/android, all
	// of which spawn a goroutine). The DB name lookup covers the
	// boot-reload window where m.viewers is still empty.
	if exists, err := m.nameExistsLocked(ctx, specKey, spec.MAC); err != nil {
		return err
	} else if exists {
		return ErrNameInUse
	}

	if err := m.insertViewerLocked(ctx, spec); err != nil {
		return err
	}

	// Spawn the mock goroutine for every viewer type (web, esp,
	// android). The type distinction matters for the auth surface
	// (web has cookie sessions on /webviewer/, esp has bearer
	// tokens on /esp/, android has bearer tokens on /webviewer/),
	// but on the UDM-facing side all three run the same Stage
	// 1+4+5+6 stack so the controller adopts them as regular
	// UA-Int-Viewers and delivers /remote_view RPCs uniformly
	// (S13-09 pattern). The Etappe-2 FCM-push path for Android
	// will attach as an ADDITIONAL push leg on top of this
	// goroutine, not as a replacement.
	if spec.Type == TypeWeb || spec.Type == TypeESP || spec.Type == TypeAndroid {
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
		return fmt.Errorf("viewermanager: delete: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 && !ok {
		return ErrViewerNotFound
	}
	return nil
}

// Rename updates the viewer's display name in place. Duplicate
// normalised names are rejected with ErrNameInUse.
func (m *Manager) Rename(ctx context.Context, mac, newName string) error {
	if newName == "" {
		return errors.New("viewermanager: name must not be empty")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	exists, err := m.nameExistsLocked(ctx, normalize.ViewerName(newName), mac)
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
		return fmt.Errorf("viewermanager: rename: %w", err)
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
// own publish path. Production callers must use RejectDoorbellOnViewer.
func (m *Manager) LookupForReject(mac string) (Viewer, error) {
	m.mu.Lock()
	entry, ok := m.viewers[mac]
	m.mu.Unlock()
	if !ok {
		return nil, ErrViewerNotFound
	}
	return entry.viewer, nil
}

// RejectDoorbellOnViewer looks up the running viewer by mock-MAC and
// asks it to publish a /call_admin_result RPC that ends the active
// doorbell call from intercomMAC. Returns ErrViewerNotFound if the
// MAC is not currently running; callers are expected to log + drop
// (the lifecycle row was already updated in doorbellcalls before
// this gets called).
//
// This is what lets the mieter "Ignorieren" / "Anruf beenden"
// endpoints silence the intercom hardware immediately instead of
// waiting for the 30-second UDM-side timeout.
func (m *Manager) RejectDoorbellOnViewer(mac, intercomMAC string) error {
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
		return fmt.Errorf("viewermanager: set password: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrViewerNotFound
	}
	return nil
}

// LookupByName finds the web viewer whose name (case-insensitive,
// umlaut-tolerant, whitespace-tolerant) matches the input.
//
// The Wohnungs-Name IS the login: a tenant typing
// "Familie Mueller 2OG", "familie mueller 2og" or "Dämgen" must
// hit the same row every time.
//
// Implementation: read every web viewer and compare in Go.
// Negligible for <1000 apartments per server; SQLite has no
// built-in for "expand German umlauts and collapse whitespace",
// so there is no usable WHERE clause.
func (m *Manager) LookupByName(ctx context.Context, name string) (*ViewerInfo, string, error) {
	target := normalize.ViewerName(name)
	if target == "" {
		return nil, "", ErrViewerNotFound
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT mac, name, service_port, type, password_hash, password_set_at,
		        COALESCE(linked_ua_user_id, ''), esp_model, esp_fw_version, device_token_hash,
		        COALESCE(paired_intercom_mac, ''), COALESCE(stream_profile, ''),
		        COALESCE(idle_view_mode, ''),
		        auto_screensaver_seconds,
		        brightness_idle, screen_off_after_sec,
		        COALESCE(language, ''),
		        history_capture_enabled,
		        COALESCE(clock_layout, ''), COALESCE(path_mode, 'auto')
		   FROM viewers
		  WHERE type = 'web'`)
	if err != nil {
		return nil, "", fmt.Errorf("viewermanager: lookup name: %w", err)
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
			&brightness, &screenOff, &info.Language, &capture,
			&info.ClockLayout, &info.PathMode); err != nil {
			return nil, "", fmt.Errorf("viewermanager: scan: %w", err)
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
		if normalize.ViewerName(info.Name) != target {
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
			info.HasDeviceToken = true
		}
		m.mu.Lock()
		if _, ok := m.viewers[info.MAC]; ok {
			info.Running = true
		}
		m.mu.Unlock()
		return &info, hashOrEmpty(hash), nil
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("viewermanager: rows: %w", err)
	}
	return nil, "", ErrViewerNotFound
}

// nameExistsLocked checks whether a row with the normalised name
// already exists. excludeMAC may be the current subject's MAC
// (used by the rename path); empty means check every row.
func (m *Manager) nameExistsLocked(ctx context.Context, target, excludeMAC string) (bool, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT mac, name FROM viewers`)
	if err != nil {
		return false, fmt.Errorf("viewermanager: name check: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var mac, name string
		if err := rows.Scan(&mac, &name); err != nil {
			return false, fmt.Errorf("viewermanager: name check scan: %w", err)
		}
		if mac == excludeMAC {
			continue
		}
		if normalize.ViewerName(name) == target {
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
		        COALESCE(linked_ua_user_id, ''), esp_model, esp_fw_version, device_token_hash,
		        COALESCE(paired_intercom_mac, ''), COALESCE(stream_profile, ''),
		        COALESCE(idle_view_mode, ''),
		        auto_screensaver_seconds,
		        brightness_idle, screen_off_after_sec,
		        COALESCE(language, ''),
		        history_capture_enabled,
		        COALESCE(clock_layout, ''), COALESCE(path_mode, 'auto')
		   FROM viewers WHERE mac = ?`, mac).
		Scan(&info.MAC, &info.Name, &port, &info.Type, &hash, &setAt,
			&info.LinkedUAUserID, &espModel, &espFW, &espHash, &info.PairedIntercomMAC,
			&info.StreamProfile, &info.IdleViewMode, &autoSec,
			&brightness, &screenOff, &info.Language, &capture,
			&info.ClockLayout, &info.PathMode)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrViewerNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("viewermanager: load info: %w", err)
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
		info.HasDeviceToken = true
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
		        COALESCE(linked_ua_user_id, ''), esp_model, esp_fw_version, device_token_hash,
		        COALESCE(paired_intercom_mac, ''), COALESCE(stream_profile, ''),
		        COALESCE(idle_view_mode, ''),
		        auto_screensaver_seconds,
		        brightness_idle, screen_off_after_sec,
		        COALESCE(language, ''),
		        history_capture_enabled,
		        COALESCE(clock_layout, ''), COALESCE(path_mode, 'auto')
		   FROM viewers ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("viewermanager: list: %w", err)
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
			&brightness, &screenOff, &info.Language, &capture,
			&info.ClockLayout, &info.PathMode); err != nil {
			return nil, fmt.Errorf("viewermanager: scan list: %w", err)
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
			info.HasDeviceToken = true
		}
		out = append(out, info)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("viewermanager: list rows: %w", err)
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
	return viewerstore.Insert(ctx, m.db.DB, viewerstore.InsertSpec{
		MAC:                    spec.MAC,
		Name:                   spec.Name,
		ServicePort:            spec.ServicePort,
		Type:                   spec.Type,
		LinkedUAUserID:         spec.LinkedUAUserID,
		ESPModel:               spec.ESPModel,
		ESPFwVersion:           spec.ESPFwVersion,
		DeviceTokenHash:           spec.DeviceTokenHash,
		PairedIntercomMAC:      spec.PairedIntercomMAC,
		StreamProfile:          spec.StreamProfile,
		IdleViewMode:           spec.IdleViewMode,
		AutoScreensaverSeconds: spec.AutoScreensaverSeconds,
		DeviceLabel:            spec.DeviceLabel,
	}, m.opts.Now().UnixMilli())
}

// setColumnExec wraps the UPDATE + updated_at + RowsAffected
// pattern shared by the per-column setter family
// (SetBrightnessIdle, SetClockLayout, SetIdleViewMode, ...).
// The caller supplies the column name as a Go string constant
// at the call site (never from user input) and the already-
// prepared DB value (int64, string, sql.NullString, nil, ...).
// The error wrapping uses `op` as a short tag so the wrapped
// message stays recognisable per call site.
func (m *Manager) setColumnExec(ctx context.Context, op, mac, column string, dbValue any) error {
	now := m.opts.Now().UnixMilli()
	res, err := m.db.ExecContext(ctx,
		`UPDATE viewers SET `+column+` = ?, updated_at = ? WHERE mac = ?`,
		dbValue, now, mac)
	if err != nil {
		return fmt.Errorf("viewermanager: %s: %w", op, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrViewerNotFound
	}
	return nil
}

// updateCachedSpec runs mutate against the cached ViewerSpec
// under m.mu if the viewer is present in the cache. No-op when
// the viewer is not cached (e.g. setter racing a RemoveViewer).
// Used by setters whose field is mirrored in the cache; setters
// for fields that live only in ViewerInfo (BrightnessIdle,
// ScreenOffAfterSec, Language, HistoryCaptureEnabled,
// ClockLayout) skip this and rely on the next GetViewerInfo to
// pick up the fresh DB value.
func (m *Manager) updateCachedSpec(mac string, mutate func(*ViewerSpec)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry, ok := m.viewers[mac]; ok {
		mutate(&entry.spec)
	}
}

// SetPairedIntercomMAC updates a viewer's paired intercom (the
// source for the standby "Tuer auf" button). Empty string clears
// the pairing - the standby button becomes inert until set
// again. The pair value is normalised to lowercase + trimmed
// before write so future LookupDoorForIntercom calls match
// regardless of how the admin typed the MAC.
func (m *Manager) SetPairedIntercomMAC(ctx context.Context, mac, intercomMAC string) error {
	normalised := strings.ToLower(strings.TrimSpace(intercomMAC))
	if err := m.setColumnExec(ctx, "set paired intercom", mac, "paired_intercom_mac", normalised); err != nil {
		return err
	}
	m.updateCachedSpec(mac, func(s *ViewerSpec) { s.PairedIntercomMAC = normalised })
	return nil
}

// DoorAssignment is one entry in a viewer's 1:n door list
// (viewer_doors, Saison 19-30). DoorID is a UA-Access door UUID;
// Label is an optional UI display override; Sort orders the list.
// This is the successor to the single paired_intercom_mac - which
// stays as the in-call auto-resolution fallback, not removed.
type DoorAssignment struct {
	DoorID string
	Label  string
	Sort   int
}

// ListViewerDoors returns the doors assigned to a viewer, ordered
// by sort then door_id. Returns an empty slice (nil error) when no
// doors are assigned.
func (m *Manager) ListViewerDoors(ctx context.Context, mac string) ([]DoorAssignment, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT door_id, label, sort
		   FROM viewer_doors
		  WHERE viewer_mac = ?
		  ORDER BY sort, door_id`, mac)
	if err != nil {
		return nil, fmt.Errorf("viewermanager: list viewer doors: %w", err)
	}
	defer rows.Close()
	out := make([]DoorAssignment, 0)
	for rows.Next() {
		var d DoorAssignment
		if err := rows.Scan(&d.DoorID, &d.Label, &d.Sort); err != nil {
			return nil, fmt.Errorf("viewermanager: scan viewer door: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("viewermanager: list viewer doors rows: %w", err)
	}
	return out, nil
}

// ViewerHasDoor reports whether door_id is assigned to the viewer.
// The mieter-unlock path uses it as the authorisation gate before a
// direct-UUID UnlockDoor: a viewer may only open doors an admin has
// assigned. door_id is matched verbatim (UA door UUIDs are stored
// as-is).
func (m *Manager) ViewerHasDoor(ctx context.Context, mac, doorID string) (bool, error) {
	doorID = strings.TrimSpace(doorID)
	if doorID == "" {
		return false, nil
	}
	var one int
	err := m.db.QueryRowContext(ctx,
		`SELECT 1 FROM viewer_doors WHERE viewer_mac = ? AND door_id = ?`,
		mac, doorID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("viewermanager: viewer has door: %w", err)
	}
	return true, nil
}

// SetViewerDoors replaces a viewer's entire door assignment with
// the given list in one transaction (delete-all then re-insert). An
// empty/nil list clears the assignment. DoorIDs are trimmed; empty
// ones are skipped. A zero Sort falls back to the slice index so a
// caller can just pass doors in display order without numbering.
func (m *Manager) SetViewerDoors(ctx context.Context, mac string, doors []DoorAssignment) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("viewermanager: set viewer doors begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM viewer_doors WHERE viewer_mac = ?`, mac); err != nil {
		return fmt.Errorf("viewermanager: set viewer doors clear: %w", err)
	}
	for i, d := range doors {
		doorID := strings.TrimSpace(d.DoorID)
		if doorID == "" {
			continue
		}
		order := d.Sort
		if order == 0 {
			order = i
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO viewer_doors (viewer_mac, door_id, label, sort)
			 VALUES (?, ?, ?, ?)`,
			mac, doorID, strings.TrimSpace(d.Label), order); err != nil {
			return fmt.Errorf("viewermanager: set viewer doors insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("viewermanager: set viewer doors commit: %w", err)
	}
	return nil
}

// AddViewerDoor adds (or updates) one door assignment. Idempotent
// on (viewer_mac, door_id).
func (m *Manager) AddViewerDoor(ctx context.Context, mac string, d DoorAssignment) error {
	doorID := strings.TrimSpace(d.DoorID)
	if doorID == "" {
		return fmt.Errorf("viewermanager: add viewer door: door_id required")
	}
	if _, err := m.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO viewer_doors (viewer_mac, door_id, label, sort)
		 VALUES (?, ?, ?, ?)`,
		mac, doorID, strings.TrimSpace(d.Label), d.Sort); err != nil {
		return fmt.Errorf("viewermanager: add viewer door: %w", err)
	}
	return nil
}

// RemoveViewerDoor deletes one door assignment. No error when the
// row does not exist.
func (m *Manager) RemoveViewerDoor(ctx context.Context, mac, doorID string) error {
	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM viewer_doors WHERE viewer_mac = ? AND door_id = ?`,
		mac, strings.TrimSpace(doorID)); err != nil {
		return fmt.Errorf("viewermanager: remove viewer door: %w", err)
	}
	return nil
}

// SetStreamProfile updates a viewer's go2rtc stream profile name.
// Empty string clears the override - the viewer falls back to the
// type-based convention (see ResolveStreamProfile). The value is
// stored trimmed; no further validation here, because go2rtc may
// hold profiles we are not aware of (and the admin UI already
// limits the input to the live profile list).
func (m *Manager) SetStreamProfile(ctx context.Context, mac, profile string) error {
	trimmed := strings.TrimSpace(profile)
	if err := m.setColumnExec(ctx, "set stream profile", mac, "stream_profile", nullable(trimmed)); err != nil {
		return err
	}
	m.updateCachedSpec(mac, func(s *ViewerSpec) { s.StreamProfile = trimmed })
	return nil
}

// SetIdleViewMode updates a viewer's idle-view-mode preference.
// Empty string clears it (next render falls back to "screensaver").
// Any non-empty value other than IdleViewModeScreensaver /
// IdleViewModeLivestream / IdleViewModeScreenOff is rejected so
// we never persist garbage that a future template lookup would
// not recognise. "screen_off" is honoured by ESP firmware
// (backlight off); web viewers render it identical to the
// screensaver.
func (m *Manager) SetIdleViewMode(ctx context.Context, mac, mode string) error {
	trimmed := strings.TrimSpace(mode)
	switch trimmed {
	case "", IdleViewModeScreensaver, IdleViewModeLivestream, IdleViewModeScreenOff:
	default:
		return fmt.Errorf("viewermanager: idle view mode %q must be %q, %q or %q",
			trimmed, IdleViewModeScreensaver, IdleViewModeLivestream, IdleViewModeScreenOff)
	}
	if err := m.setColumnExec(ctx, "set idle view mode", mac, "idle_view_mode", nullable(trimmed)); err != nil {
		return err
	}
	m.updateCachedSpec(mac, func(s *ViewerSpec) { s.IdleViewMode = trimmed })
	return nil
}

// SetBrightnessIdle persists the ESP idle-brightness (range
// 0..100). Values outside the range are rejected.
func (m *Manager) SetBrightnessIdle(ctx context.Context, mac string, value int) error {
	if value < 0 || value > 100 {
		return fmt.Errorf("viewermanager: brightness_idle %d must be in 0..100", value)
	}
	return m.setColumnExec(ctx, "set brightness idle", mac, "brightness_idle", int64(value))
}

// SetScreenOffAfterSec persists the ESP backlight-off timer.
// Values outside ScreenOffAfterSecAllowed are rejected. 0
// disables the feature and stores SQL NULL.
func (m *Manager) SetScreenOffAfterSec(ctx context.Context, mac string, value int) error {
	allowed := false
	for _, v := range ScreenOffAfterSecAllowed {
		if v == value {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("viewermanager: screen_off_after_sec %d not in %v",
			value, ScreenOffAfterSecAllowed)
	}
	var stored any
	if value > 0 {
		stored = int64(value)
	}
	return m.setColumnExec(ctx, "set screen off after sec", mac, "screen_off_after_sec", stored)
}

// SetClockLayout persists the tenant preference for the
// screensaver clock layout. Values are strictly checked against
// ClockLayoutAllowed; anything else returns an error.
func (m *Manager) SetClockLayout(ctx context.Context, mac, value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed != "" {
		allowed := false
		for _, v := range ClockLayoutAllowed {
			if v == trimmed {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("viewermanager: clock_layout %q not in %v",
				trimmed, ClockLayoutAllowed)
		}
	}
	return m.setColumnExec(ctx, "set clock layout", mac, "clock_layout", nullable(trimmed))
}

// SetPathMode persists the per-viewer transport-path override
// (WEG-Schalter, Saison 19-39). Strictly checked against
// PathModeAllowed; anything else is an error. Empty normalises to
// "auto". The column is NOT NULL DEFAULT 'auto', so the value is
// always written non-null.
func (m *Manager) SetPathMode(ctx context.Context, mac, value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		trimmed = PathModeAuto
	}
	allowed := false
	for _, v := range PathModeAllowed {
		if v == trimmed {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("viewermanager: path_mode %q not in %v", trimmed, PathModeAllowed)
	}
	return m.setColumnExec(ctx, "set path mode", mac, "path_mode", trimmed)
}

// ListViewerSettingVisibility returns the EXPLICIT per-setting
// visibility rows for a viewer (setting_key -> visible). A setting with
// NO row is visible by default, so the map carries only what the admin
// explicitly set; callers treat a missing key as visible. (Saison 19-39)
func (m *Manager) ListViewerSettingVisibility(ctx context.Context, mac string) (map[string]bool, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT setting_key, visible_to_tenant FROM viewer_setting_visibility WHERE viewer_mac = ?`, mac)
	if err != nil {
		return nil, fmt.Errorf("viewermanager: list setting visibility: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var key string
		var vis int
		if err := rows.Scan(&key, &vis); err != nil {
			return nil, fmt.Errorf("viewermanager: scan setting visibility: %w", err)
		}
		out[key] = vis != 0
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("viewermanager: list setting visibility rows: %w", err)
	}
	return out, nil
}

// SetViewerSettingVisibility upserts the visibility of one setting for
// one viewer (Saison 19-39). visible=true is stored explicitly as 1 (the
// row is not deleted) so the admin's intent is recorded even when it
// matches the default. setting_key is free-text (premium-extensible).
func (m *Manager) SetViewerSettingVisibility(ctx context.Context, mac, settingKey string, visible bool) error {
	settingKey = strings.TrimSpace(settingKey)
	if settingKey == "" {
		return fmt.Errorf("viewermanager: set setting visibility: setting_key required")
	}
	v := 0
	if visible {
		v = 1
	}
	if _, err := m.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO viewer_setting_visibility (viewer_mac, setting_key, visible_to_tenant)
		 VALUES (?, ?, ?)`, mac, settingKey, v); err != nil {
		return fmt.Errorf("viewermanager: set setting visibility: %w", err)
	}
	return nil
}

// SetHistoryCaptureEnabled persists the tenant privacy toggle.
// true = tenant sees the history again; false = the tenant API
// returns an empty list with capture_enabled=false. Admin paths
// are unaffected - the toggle only changes what the tenant UI
// renders, the server still writes door_events rows so the audit
// trail remains complete.
func (m *Manager) SetHistoryCaptureEnabled(ctx context.Context, mac string, enabled bool) error {
	var stored int64
	if enabled {
		stored = 1
	}
	return m.setColumnExec(ctx, "set history capture", mac, "history_capture_enabled", stored)
}

// SetLanguage persists the UI language. Values outside the
// allow-list are rejected. An empty value is allowed and means
// "reset to default" (NULL in the DB).
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
			return fmt.Errorf("viewermanager: language %q not in %v",
				trimmed, LanguageAllowed)
		}
	}
	return m.setColumnExec(ctx, "set language", mac, "language", nullable(trimmed))
}

// SetFCMToken persists the device's Firebase Cloud Messaging
// token on its viewers row (Saison 16 FCM Etappe). An empty
// token clears the column (NULL) - used on logout so a signed-
// out device no longer receives push. Pure DB setter, no cache
// update: fcm_token is not mirrored in ViewerSpec, and the
// (yet-undecided) push-send path will read it fresh from the DB
// when a doorbell fires.
//
// This step only stores the token. WHERE the FCM send later
// happens (RPi-direct vs. the planned cloud server) is
// deliberately not decided here.
func (m *Manager) SetFCMToken(ctx context.Context, mac, token string) error {
	return m.setColumnExec(ctx, "set fcm token", mac, "fcm_token", nullable(strings.TrimSpace(token)))
}

// GetFCMToken reads the device's FCM token from its viewers row. A
// NULL or empty column returns "" with no error - the caller treats
// empty as "no phone registered for this viewer, skip the push". A
// missing row returns ErrViewerNotFound. Read fresh from the DB on
// every call; fcm_token is deliberately not mirrored in ViewerSpec
// (see SetFCMToken), so the doorbell send path always sees the
// current value.
func (m *Manager) GetFCMToken(ctx context.Context, mac string) (string, error) {
	var token string
	err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(fcm_token, '') FROM viewers WHERE mac = ?`, mac).
		Scan(&token)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrViewerNotFound
	}
	if err != nil {
		return "", fmt.Errorf("viewermanager: get fcm token: %w", err)
	}
	return token, nil
}

// AutoScreensaverSecondsAllowed is the closed set of values the
// inline-settings form may persist. 0 means "off" and is stored
// as SQL NULL; the others are seconds.
var AutoScreensaverSecondsAllowed = []int{0, 30, 60, 300, 600}

// SetAutoScreensaverSeconds updates the auto-fallback timer for
// the given viewer. Pass 0 to disable the timer (the column
// becomes NULL); pass any of {30, 60, 300, 600} to enable it.
// Other values are rejected up-front so a future regression on
// the POST handler does not let arbitrary integers reach the
// browser runtime.
func (m *Manager) SetAutoScreensaverSeconds(ctx context.Context, mac string, seconds int) error {
	allowed := false
	for _, v := range AutoScreensaverSecondsAllowed {
		if v == seconds {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("viewermanager: auto_screensaver_seconds %d not in %v",
			seconds, AutoScreensaverSecondsAllowed)
	}
	var stored any
	if seconds > 0 {
		stored = int64(seconds)
	}
	if err := m.setColumnExec(ctx, "set auto screensaver", mac, "auto_screensaver_seconds", stored); err != nil {
		return err
	}
	m.updateCachedSpec(mac, func(s *ViewerSpec) {
		if seconds > 0 {
			v := seconds
			s.AutoScreensaverSeconds = &v
		} else {
			s.AutoScreensaverSeconds = nil
		}
	})
	return nil
}

// SiblingDeviceMACs liefert alle ESP-Viewer-MACs die an demselben
// UA-User haengen wie der uebergebene MAC, ausser dem MAC selbst.
// Wird vom /esp/answer-Pfad genutzt um "answered elsewhere"-
// Cancel-Events an die anderen Geraete des Mieters zu pushen.
// Wenn der Viewer keine linked_ua_user_id hat, gibt es per
// Definition keine Siblings (leere Liste, kein Fehler).
func (m *Manager) SiblingDeviceMACs(ctx context.Context, mac string) ([]string, error) {
	var linked sql.NullString
	err := m.db.QueryRowContext(ctx,
		`SELECT linked_ua_user_id FROM viewers WHERE mac = ?`, mac).Scan(&linked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrViewerNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("viewermanager: sibling lookup self: %w", err)
	}
	if !linked.Valid || linked.String == "" {
		return nil, nil
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT mac FROM viewers
		  WHERE type = 'esp' AND linked_ua_user_id = ? AND mac <> ?`,
		linked.String, mac)
	if err != nil {
		return nil, fmt.Errorf("viewermanager: sibling query: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var sibling string
		if err := rows.Scan(&sibling); err != nil {
			return nil, fmt.Errorf("viewermanager: sibling scan: %w", err)
		}
		out = append(out, sibling)
	}
	return out, rows.Err()
}

// TouchESPSeen only updates updated_at for an ESP viewer. Used by
// the /esp/heartbeat fallback and the /esp/state endpoint so the
// admin dashboard can render a "zuletzt gesehen" without every
// poll touching other columns.
func (m *Manager) TouchESPSeen(ctx context.Context, mac string) error {
	now := m.opts.Now().UnixMilli()
	res, err := m.db.ExecContext(ctx,
		`UPDATE viewers SET updated_at = ? WHERE mac = ? AND type = 'esp'`,
		now, mac)
	if err != nil {
		return fmt.Errorf("viewermanager: touch esp: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrViewerNotFound
	}
	return nil
}

// SetDeviceTokenHash stores a freshly generated token hash for an
// adopted ESP or Android viewer (both carry a device_token_hash and
// share the bearer-token mechanic). The previous token-hash row is
// overwritten (token rotation).
func (m *Manager) SetDeviceTokenHash(ctx context.Context, mac, hash string) error {
	now := m.opts.Now().UnixMilli()
	res, err := m.db.ExecContext(ctx,
		`UPDATE viewers SET device_token_hash = ?, updated_at = ?
		 WHERE mac = ? AND type IN ('esp', 'android')`,
		nullable(hash), now, mac)
	if err != nil {
		return fmt.Errorf("viewermanager: set esp token: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrViewerNotFound
	}
	return nil
}

// LookupDeviceMACByToken compares the clear-text bearer token
// presented by an ESP against every adopted ESP viewer and
// returns the MAC of the matching device. Verify uses
// crypto/subtle.ConstantTimeCompare. With <100 ESP viewers per
// server (realistic for a single residential complex), the
// linear-scan strategy is cheap enough; this can switch to an
// indexed hash lookup once a deployment grows into the
// multi-tenant range.
func (m *Manager) LookupDeviceMACByToken(ctx context.Context, presented string) (string, error) {
	if presented == "" {
		return "", ErrViewerNotFound
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT mac, device_token_hash FROM viewers
		  WHERE type IN ('esp', 'android')
		    AND device_token_hash IS NOT NULL`)
	if err != nil {
		return "", fmt.Errorf("viewermanager: lookup esp by token: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var mac string
		var hash sql.NullString
		if err := rows.Scan(&mac, &hash); err != nil {
			return "", fmt.Errorf("viewermanager: scan esp token: %w", err)
		}
		if !hash.Valid || hash.String == "" {
			continue
		}
		if esptoken.Verify(presented, hash.String) {
			return mac, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("viewermanager: rows: %w", err)
	}
	return "", ErrViewerNotFound
}

// LookupDeviceTokenHash returns the token hash for an adopted ESP
// viewer. Used by the bearer-auth middleware on the /esp/ tree
// and by the discovery status-poll logic.
func (m *Manager) LookupDeviceTokenHash(ctx context.Context, mac string) (string, error) {
	var hash sql.NullString
	err := m.db.QueryRowContext(ctx,
		`SELECT device_token_hash FROM viewers WHERE mac = ? AND type = 'esp'`,
		mac).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrViewerNotFound
	}
	if err != nil {
		return "", fmt.Errorf("viewermanager: lookup esp token: %w", err)
	}
	return hashOrEmpty(hash), nil
}

// SetLinkedUAUserID updates the optional UA-user link. An empty
// userID clears the link. Used by the web-viewer edit path.
func (m *Manager) SetLinkedUAUserID(ctx context.Context, mac, userID string) error {
	now := m.opts.Now().UnixMilli()
	res, err := m.db.ExecContext(ctx,
		`UPDATE viewers SET linked_ua_user_id = ?, updated_at = ? WHERE mac = ?`,
		nullable(userID), now, mac)
	if err != nil {
		return fmt.Errorf("viewermanager: set linked ua user: %w", err)
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
		return fmt.Errorf("viewermanager: factory: %w", err)
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
					"mac", ev.ViewerMAC,
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
					"mac", ev.ViewerMAC,
					"cancel_token", ev.CancelToken,
				)
			}
		}
	}
}

func validateSpec(spec ViewerSpec) error {
	if spec.MAC == "" {
		return errors.New("viewermanager: MAC must not be empty")
	}
	if spec.Name == "" {
		return errors.New("viewermanager: Name must not be empty")
	}
	if spec.ServicePort == 0 {
		return errors.New("viewermanager: ServicePort must be > 0")
	}
	if spec.Type != "" && spec.Type != TypeWeb && spec.Type != TypeESP && spec.Type != TypeAndroid {
		return fmt.Errorf("viewermanager: Type %q must be 'web', 'esp' or 'android'", spec.Type)
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
