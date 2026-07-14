package mideamonitor

// Control-loop (E2) support: the Logic Editor's control_loop block drives a
// device through the monitor's SAME live connection as the cockpit. Two pieces:
//
//   - an "under automatic control" ref-count per device, set by the run builder
//     while a control_loop run is active, so the cockpit and the device block's
//     manual control are locked out (single-driver exclusivity);
//   - an async apply queue: the control_loop's tick MUST NOT block on device
//     I/O, so it enqueues a full control decision here and a worker applies it
//     (with the same reconnect+retry as the cockpit).

import (
	"context"
	"time"

	"carvilon.local/server/internal/mideaclimate"
	"carvilon.local/server/internal/mideastore"
)

// ClimateStatus implements mideaengine.Device: the bound device's own return-air
// sensor plus whether the control loop may drive it (adopted AND advanced
// profile). Read from the monitor cache, non-blocking - safe in the engine tick.
func (m *Monitor) ClimateStatus(id string) (deviceTemp float64, hasDevice bool, enabled bool) {
	r, ok := m.Get(id)
	if !ok {
		return 0, false, false
	}
	enabled = r.Profile == mideastore.ProfileAdvanced
	if r.HasTemp {
		return r.DeviceTempC, true, enabled
	}
	return 0, false, enabled
}

// Drive implements mideaengine.Device: queue a control decision (non-blocking).
func (m *Monitor) Drive(id string, out mideaclimate.Outputs) { m.QueueApply(id, out) }

// SetAutomatic marks (on=true) or unmarks a device as under control_loop
// automatic control. Ref-counted so overlapping runs compose. The run builder
// calls it at bind (on) and teardown (off).
func (m *Monitor) SetAutomatic(id string, on bool) {
	if m == nil || id == "" {
		return
	}
	m.mu.Lock()
	if on {
		m.automatic[id]++
	} else if m.automatic[id] > 0 {
		m.automatic[id]--
		if m.automatic[id] == 0 {
			delete(m.automatic, id)
		}
	}
	m.mu.Unlock()
}

// IsAutomatic reports whether a control_loop run is currently driving the
// device. The cockpit + device-block sink consult it and defer.
func (m *Monitor) IsAutomatic(id string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.automatic[id] > 0
}

// QueueApply enqueues a full control decision (setpoint+mode+fan) for the
// worker. Non-blocking: safe to call from inside the engine tick.
func (m *Monitor) QueueApply(id string, out mideaclimate.Outputs) {
	if m == nil || id == "" {
		return
	}
	select {
	case m.applyQueue <- queuedApply{id: id, out: out}:
	default:
		m.log.Warn("midea control_loop: apply queue full, dropping", "id", id)
	}
}

// Apply drives one full control decision to the device in a single round-trip
// (setpoint+mode+fan together), with the shared reconnect+retry.
func (m *Monitor) Apply(ctx context.Context, id string, out mideaclimate.Outputs) error {
	return m.control(ctx, id, func(dev *mideaclimate.Device) error {
		return dev.Control(ctx, out)
	})
}

// applyWorker drains queued control-loop decisions off the tick path.
func (m *Monitor) applyWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case qa := <-m.applyQueue:
			actx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			if err := m.Apply(actx, qa.id, qa.out); err != nil {
				m.log.Warn("midea control_loop: apply failed", "id", qa.id, "err", err)
			}
			cancel()
		}
	}
}
