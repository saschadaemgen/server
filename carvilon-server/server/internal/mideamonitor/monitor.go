// Package mideamonitor keeps the adopted Midea split-AC units connected and
// polled, and proxies the standard-profile control commands (set temperature /
// mode / fan) to the device via the mideaclimate adapter.
//
// It is the runtime half of the Midea Climate Controller device family: the
// store (internal/mideastore) owns which devices exist and their encrypted
// credentials; this monitor owns the live TCP connections and the cached last
// status the Device Center renders. On startup (and whenever a device is
// adopted or removed) it reconciles the connected set against the store's active
// set, so an adopted device is re-provisioned from persisted credentials after a
// server restart - no cloud round-trip, the device keeps running on its own
// internal sensor throughout ("survives a server outage").
//
// The standard profile is device-side control: there is no server control loop
// here. Every command is a direct passthrough to the adapter, exactly like a
// remote control.
package mideamonitor

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	"carvilon.local/server/internal/mideaclimate"
	"carvilon.local/server/internal/mideastore"
)

// Store is the subset of *mideastore.Store the monitor needs.
type Store interface {
	ListActive(ctx context.Context) ([]mideastore.Device, error)
	Get(ctx context.Context, id string) (mideastore.Device, error)
	Credential(ctx context.Context, id string) (token, key []byte, err error)
}

// Readout is the cached, display-ready status of one adopted device.
type Readout struct {
	ID           string
	Address      string
	Online       bool
	Provisioning bool
	Power        bool
	Mode         string // "off" | "cool" | "heat" | "dry" | "fan_only"
	Setpoint     float64
	Fan          string  // "auto" | "low" | "mid" | "high"
	DeviceTempC  float64 // device return-air sensor
	HasTemp      bool
	OutdoorC     float64
	HasOutdoor   bool
	Profile      string // standard | advanced
	Automatic    bool   // a control_loop run is currently driving this device
	LastErr      string // last poll/connection error
	LastCtrlErr  string // last control command error (surfaced in the cockpit)
	LastPollMS   int64
}

type devState struct {
	id           string
	address      string
	deviceID     uint64
	profile      string // mideastore profile (standard | advanced)
	dev          *mideaclimate.Device
	provisioning bool
	online       bool
	last         mideaclimate.State
	hasState     bool
	lastErr      string
	lastCtrlErr  string
	lastPollMS   int64
}

// Monitor holds the live device connections + cached status.
type Monitor struct {
	store Store
	log   *slog.Logger

	interval       time.Duration
	connectTimeout time.Duration
	pollTimeout    time.Duration

	mu        sync.Mutex
	devs      map[string]*devState
	trigger   chan struct{}
	bindings  map[*RunBinding]struct{} // live Logic-Editor run drivers (readout push)
	automatic map[string]int           // ref-count of active control_loop runs per device
	now       func() time.Time

	// applyQueue carries full control decisions (setpoint+mode+fan) from the
	// control_loop run driver to a worker, so the engine tick never blocks on
	// device I/O.
	applyQueue chan queuedApply
}

type queuedApply struct {
	id  string
	out mideaclimate.Outputs
}

// Option mutates a Monitor during construction.
type Option func(*Monitor)

// WithInterval sets the poll interval (default 20s).
func WithInterval(d time.Duration) Option { return func(m *Monitor) { m.interval = d } }

// WithClock injects a test clock.
func WithClock(now func() time.Time) Option { return func(m *Monitor) { m.now = now } }

// New constructs a Monitor.
func New(store Store, log *slog.Logger, opts ...Option) *Monitor {
	if log == nil {
		log = slog.Default()
	}
	m := &Monitor{
		store:          store,
		log:            log.With("component", "mideamonitor"),
		interval:       15 * time.Second, // also a keepalive: Midea drops idle TCP
		connectTimeout: 6 * time.Second,
		pollTimeout:    5 * time.Second,
		devs:           make(map[string]*devState),
		trigger:        make(chan struct{}, 1),
		bindings:       make(map[*RunBinding]struct{}),
		automatic:      make(map[string]int),
		applyQueue:     make(chan queuedApply, 64),
		now:            time.Now,
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Run reconciles + polls until ctx is cancelled, then closes every connection.
// A no-op when the store is nil.
func (m *Monitor) Run(ctx context.Context) {
	if m == nil || m.store == nil {
		return
	}
	go m.applyWorker(ctx) // drains control_loop drive commands off the tick path
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		m.tick(ctx)
		select {
		case <-ctx.Done():
			m.closeAll()
			return
		case <-ticker.C:
		case <-m.trigger:
		}
	}
}

// Refresh nudges the monitor to reconcile now (after an adopt / remove), without
// waiting for the next tick. Non-blocking.
func (m *Monitor) Refresh() {
	if m == nil {
		return
	}
	select {
	case m.trigger <- struct{}{}:
	default:
	}
}

// tick reconciles the connected set against the store's active set, then polls
// the connected devices and (re)provisions the disconnected ones.
func (m *Monitor) tick(ctx context.Context) {
	active, err := m.store.ListActive(ctx)
	if err != nil {
		m.log.Warn("list active failed", "err", err)
		return
	}
	activeByID := make(map[string]mideastore.Device, len(active))
	for _, d := range active {
		activeByID[d.ID] = d
	}

	// Drop devices no longer active; ensure a devState per active device.
	var toClose []*mideaclimate.Device
	var toPoll []*devState
	var toProvision []mideastore.Device
	m.mu.Lock()
	for id, ds := range m.devs {
		if _, ok := activeByID[id]; !ok {
			if ds.dev != nil {
				toClose = append(toClose, ds.dev)
			}
			delete(m.devs, id)
		}
	}
	for id, d := range activeByID {
		ds := m.devs[id]
		if ds == nil {
			ds = &devState{id: id, address: d.Address, deviceID: d.DeviceID}
			m.devs[id] = ds
		} else {
			ds.address, ds.deviceID = d.Address, d.DeviceID
		}
		ds.profile = d.Profile
		switch {
		case ds.dev != nil:
			toPoll = append(toPoll, ds)
		case !ds.provisioning:
			ds.provisioning = true
			toProvision = append(toProvision, d)
		}
	}
	m.mu.Unlock()

	for _, dev := range toClose {
		_ = dev.Deprovision(context.Background())
	}
	for _, d := range toProvision {
		go m.provisionAsync(ctx, d)
	}
	for _, ds := range toPoll {
		m.pollOne(ctx, ds)
	}
}

// provisionAsync connects one adopted device from its persisted credentials,
// then polls it once immediately so the cockpit readouts populate without
// waiting a full interval (and the fresh connection is exercised right away).
func (m *Monitor) provisionAsync(ctx context.Context, d mideastore.Device) {
	dev, err := m.connect(ctx, d)
	m.mu.Lock()
	ds := m.devs[d.ID]
	if ds == nil { // removed while connecting
		m.mu.Unlock()
		if dev != nil {
			go dev.Deprovision(context.Background())
		}
		return
	}
	ds.provisioning = false
	ds.lastPollMS = m.now().UnixMilli()
	if err != nil {
		ds.online = false
		ds.lastErr = err.Error()
		m.mu.Unlock()
		m.log.Warn("provision failed", "id", d.ID, "err", err)
		return
	}
	// ensure() may have connected + installed a handle on demand (a control
	// command during our connect window); keep it and close ours so we do not
	// overwrite and leak a live connection.
	if ds.dev != nil {
		m.mu.Unlock()
		go dev.Deprovision(context.Background())
		return
	}
	ds.dev = dev
	ds.online = true
	ds.lastErr = ""
	m.mu.Unlock()
	// Immediate first poll (lock released) so status is fresh right after adopt.
	m.pollOne(ctx, ds)
}

// connect provisions a device handle from the store's encrypted credentials.
func (m *Monitor) connect(ctx context.Context, d mideastore.Device) (*mideaclimate.Device, error) {
	token, key, err := m.store.Credential(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	creds := mideaclimate.Credentials{
		IP:       hostOnly(d.Address),
		DeviceID: d.DeviceID,
		Token:    token,
		Key:      key,
	}
	cctx, cancel := context.WithTimeout(ctx, m.connectTimeout)
	defer cancel()
	return mideaclimate.Provision(cctx, d.Address, creds)
}

// pollOne reads live status from a connected device, updating its cache. On
// error it drops the handle so the next tick re-provisions.
func (m *Monitor) pollOne(ctx context.Context, ds *devState) {
	m.mu.Lock()
	dev := ds.dev
	m.mu.Unlock()
	if dev == nil {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, m.pollTimeout)
	st, err := dev.Status(pctx)
	cancel()

	m.mu.Lock()
	ds.lastPollMS = m.now().UnixMilli()
	// The handle may have been dropped (a failed control) or replaced (ensure)
	// while we polled. If so, our result is stale and whoever swapped it already
	// owns closing our handle - do not touch ds.dev or double-Deprovision.
	if ds.dev != dev {
		m.mu.Unlock()
		return
	}
	if err != nil {
		ds.online = false
		ds.lastErr = err.Error()
		ds.dev = nil
		m.mu.Unlock()
		go dev.Deprovision(context.Background())
		return
	}
	ds.last = st
	ds.hasState = true
	ds.online = true
	ds.lastErr = ""
	m.mu.Unlock()
	// Feed the fresh readouts to any Logic-Editor run drivers.
	m.pushReadouts(ds.id)
}

// Snapshot returns the cached status of every adopted device.
func (m *Monitor) Snapshot() []Readout {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Readout, 0, len(m.devs))
	for _, ds := range m.devs {
		r := ds.readout()
		r.Automatic = m.automatic[ds.id] > 0
		out = append(out, r)
	}
	return out
}

// Get returns the cached status of one device.
func (m *Monitor) Get(id string) (Readout, bool) {
	if m == nil {
		return Readout{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ds, ok := m.devs[id]
	if !ok {
		return Readout{}, false
	}
	r := ds.readout()
	r.Automatic = m.automatic[id] > 0
	return r, true
}

func (ds *devState) readout() Readout {
	r := Readout{
		ID:           ds.id,
		Address:      ds.address,
		Online:       ds.online,
		Provisioning: ds.provisioning,
		Profile:      ds.profile,
		LastErr:      ds.lastErr,
		LastCtrlErr:  ds.lastCtrlErr,
		LastPollMS:   ds.lastPollMS,
	}
	if ds.hasState {
		r.Power = ds.last.Power
		r.Mode = string(ds.last.Mode)
		r.Setpoint = ds.last.Setpoint
		r.Fan = string(ds.last.Fan)
		r.DeviceTempC, r.HasTemp = ds.last.DeviceTempC, ds.last.HasTemp
		r.OutdoorC, r.HasOutdoor = ds.last.OutdoorC, ds.last.HasOutdoor
	}
	return r
}

// SetTemperature / SetMode / SetFan proxy the standard-profile commands.

// SetTemperature sends a target temperature (17-30 C, 0.5 step; the device
// regulates on its own internal sensor).
func (m *Monitor) SetTemperature(ctx context.Context, id string, tempC float64) error {
	return m.control(ctx, id, func(dev *mideaclimate.Device) error {
		return dev.SetTemperature(ctx, tempC)
	})
}

// SetMode switches the operating mode (off/cool/heat/dry/fan_only/auto).
func (m *Monitor) SetMode(ctx context.Context, id, mode string) error {
	md := parseMode(mode)
	return m.control(ctx, id, func(dev *mideaclimate.Device) error {
		return dev.SetMode(ctx, md)
	})
}

// SetFan selects the fan step (auto/low/mid/high).
func (m *Monitor) SetFan(ctx context.Context, id, fan string) error {
	f := parseFan(fan)
	return m.control(ctx, id, func(dev *mideaclimate.Device) error {
		return dev.SetFan(ctx, f)
	})
}

// control runs a standard-profile command and RETRIES ONCE on failure with a
// fresh connection: Midea units drop idle TCP and the protocol has no keepalive,
// so the monitor's shared handle can be stale by the time the operator clicks -
// a single reconnect is what "keep a live client" means in practice. The final
// result (ok or the error text) is recorded so the cockpit can surface it
// instead of silently doing nothing.
func (m *Monitor) control(ctx context.Context, id string, fn func(*mideaclimate.Device) error) error {
	err := m.attempt(ctx, id, fn)
	if err != nil {
		err = m.attempt(ctx, id, fn) // reconnect + retry once (handle was dropped)
	}
	m.mu.Lock()
	if ds := m.devs[id]; ds != nil {
		if err != nil {
			ds.lastCtrlErr = err.Error()
		} else {
			ds.lastCtrlErr = ""
		}
	}
	m.mu.Unlock()
	if err != nil {
		m.log.Warn("control failed after retry", "id", id, "err", err)
	}
	return err
}

// attempt runs fn against a (re)connected handle once; on failure it drops the
// handle so the retry / next tick reconnects. On success it refreshes the cache.
func (m *Monitor) attempt(ctx context.Context, id string, fn func(*mideaclimate.Device) error) error {
	dev, err := m.ensure(ctx, id)
	if err != nil {
		return err
	}
	if err := fn(dev); err != nil {
		m.drop(id, dev)
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, m.pollTimeout)
	st, serr := dev.Status(cctx)
	cancel()
	if serr == nil {
		m.mu.Lock()
		if ds := m.devs[id]; ds != nil {
			ds.last, ds.hasState, ds.online, ds.lastErr = st, true, true, ""
			ds.lastPollMS = m.now().UnixMilli()
		}
		m.mu.Unlock()
	}
	return nil
}

// ensure returns a connected handle for id, provisioning one from the store if
// the device is not currently connected.
func (m *Monitor) ensure(ctx context.Context, id string) (*mideaclimate.Device, error) {
	m.mu.Lock()
	if ds := m.devs[id]; ds != nil && ds.dev != nil {
		dev := ds.dev
		m.mu.Unlock()
		return dev, nil
	}
	m.mu.Unlock()

	d, err := m.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	dev, err := m.connect(ctx, d)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	ds := m.devs[id]
	if ds == nil {
		// The devState is absent: either tick has not registered a just-approved
		// device yet, or the device was removed while we connected. Disambiguate
		// against the store so a removed device is not resurrected with a live
		// handle (it would otherwise linger until the next reconcile reaps it).
		m.mu.Unlock()
		if _, gerr := m.store.Get(ctx, id); gerr != nil {
			go dev.Deprovision(context.Background())
			return nil, gerr
		}
		m.mu.Lock()
		ds = m.devs[id]
		if ds == nil {
			ds = &devState{id: id, address: d.Address, deviceID: d.DeviceID}
			m.devs[id] = ds
		}
	}
	// If a concurrent connect already installed a handle, keep it and close ours.
	if ds.dev != nil {
		existing := ds.dev
		m.mu.Unlock()
		go dev.Deprovision(context.Background())
		return existing, nil
	}
	ds.dev = dev
	ds.online = true
	ds.provisioning = false
	m.mu.Unlock()
	return dev, nil
}

// drop discards the connected handle for id (after a control error) so the next
// tick re-provisions it.
func (m *Monitor) drop(id string, dev *mideaclimate.Device) {
	m.mu.Lock()
	if ds := m.devs[id]; ds != nil && ds.dev == dev {
		ds.dev = nil
		ds.online = false
	}
	m.mu.Unlock()
	if dev != nil {
		go dev.Deprovision(context.Background())
	}
}

func (m *Monitor) closeAll() {
	m.mu.Lock()
	devs := make([]*mideaclimate.Device, 0, len(m.devs))
	for _, ds := range m.devs {
		if ds.dev != nil {
			devs = append(devs, ds.dev)
			ds.dev = nil
			ds.online = false
		}
	}
	m.mu.Unlock()
	for _, dev := range devs {
		_ = dev.Deprovision(context.Background())
	}
}

func parseMode(s string) mideaclimate.Mode {
	switch s {
	case "cool":
		return mideaclimate.ModeCool
	case "heat":
		return mideaclimate.ModeHeat
	case "dry":
		return mideaclimate.ModeDry
	case "fan_only":
		return mideaclimate.ModeFanOnly
	case "auto":
		return mideaclimate.ModeAuto
	case "off":
		return mideaclimate.ModeOff
	default:
		return mideaclimate.ModeCool
	}
}

func parseFan(s string) mideaclimate.FanMode {
	switch s {
	case "low":
		return mideaclimate.FanLow
	case "mid":
		return mideaclimate.FanMid
	case "high":
		return mideaclimate.FanHigh
	default:
		return mideaclimate.FanAuto
	}
}

// hostOnly strips an optional :port from a stored address, leaving the bare IP
// the Midea adapter dials (the protocol uses its own fixed TCP port).
func hostOnly(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}
