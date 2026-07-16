package mqttbroker

import (
	"time"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"
)

// ReadingTap observes every device publish under the device trees. It is the
// seam the sensor-history path (Shelly recording) rides on: the devices
// already publish their live values to this broker, so recording taps that
// existing stream rather than adding a second, independent poll.
//
// CONTRACT - the tap runs ON THE BROKER'S PUBLISH PATH, synchronously, in the
// publishing client's goroutine. It must never block: anything slow here would
// delay delivery to real subscribers, which is exactly the LIVE path the
// two-path principle says recording may never throttle.
// sensorhistory.Recorder.Record satisfies this (it drops rather than blocks).
type ReadingTap func(topic string, payload []byte, at time.Time)

// SetReadingTap installs the device-message tap, or clears it when t is nil.
// Safe to call at any time, including before Start and while running: the hook
// is added on every broker start and reads the current tap through an atomic,
// so neither startup ordering nor a live reconfigure can strand it. (The
// broker is constructed and started early in main, long before the device
// stores and the history recorder the tap needs exist.)
func (m *Manager) SetReadingTap(t ReadingTap) {
	if t == nil {
		m.readingTap.Store(nil)
		return
	}
	m.readingTap.Store(&t)
}

// readingTapFn returns the installed tap, or nil.
func (m *Manager) readingTapFn() ReadingTap {
	if p := m.readingTap.Load(); p != nil {
		return *p
	}
	return nil
}

// readingsHook forwards device publishes to the ReadingTap. It mirrors
// monitorHook: same topic scoping (the device trees only, never $SYS), and
// wired on every broker start so a reconfigure keeps recording alive.
//
// OnPublished fires on a real publish only - a retained message replayed to a
// newly subscribing client is not a publish - so the tap never sees a stale
// retained value replayed as if it had just been measured.
type readingsHook struct {
	mqtt.HookBase
	m *Manager
}

func (h *readingsHook) ID() string { return "carvilon-readings-tap" }

func (h *readingsHook) Provides(b byte) bool { return b == mqtt.OnPublished }

func (h *readingsHook) OnPublished(cl *mqtt.Client, pk packets.Packet) {
	if !monitoredTopic(pk.TopicName) {
		return
	}
	fn := h.m.readingTapFn()
	if fn == nil {
		return
	}
	fn(pk.TopicName, pk.Payload, time.UnixMilli(packetTimeMillis(pk)))
}
