package mqttbroker

import (
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"
)

// maxMonitorPayload caps how many payload bytes the device monitor
// forwards per message, bounding the SSE line size. Device telemetry
// (a Shelly status/sys JSON blob) sits well under this; anything larger
// is truncated with a marker rather than streamed whole.
const maxMonitorPayload = 4096

// MonitorMessage is one publish under the carvilon/ tree, carried to the
// device-monitoring page with its FULL (bounded) payload. It is distinct
// from console.Event, whose payload is a 256-byte preview for the raw
// traffic log: the monitor needs the whole value to render a device's
// live topic tree.
type MonitorMessage struct {
	Topic     string `json:"topic"`
	Payload   string `json:"payload"`   // printable UTF-8, or a note when Binary
	Binary    bool   `json:"binary"`    // payload was not valid UTF-8
	Retain    bool   `json:"retain"`    // published (or stored) with the retain flag
	Truncated bool   `json:"truncated"` // payload was clipped to maxMonitorPayload
	Time      int64  `json:"t"`         // unix milliseconds
}

// Monitor is a fan-out hub for full-payload publishes under carvilon/#,
// feeding the device-monitoring page's live SSE. Unlike Console it keeps
// no backlog ring: a subscriber seeds from the broker's retained set
// (Manager.Retained) and then applies live deltas, so the per-topic
// latest state is always correct without replaying history. It mirrors
// the Console's subscribe/fan-out shape (and the engine monitor's).
type Monitor struct {
	mu     sync.Mutex
	subs   map[int]chan MonitorMessage
	nextID int
}

// NewMonitor returns an empty fan-out hub.
func NewMonitor() *Monitor { return &Monitor{subs: map[int]chan MonitorMessage{}} }

// publish fans a message out to live subscribers. A slow subscriber
// drops the message rather than blocking the broker's publish path; it
// re-seeds from the retained snapshot on its next reconnect.
func (mo *Monitor) publish(m MonitorMessage) {
	if mo == nil {
		return
	}
	mo.mu.Lock()
	for _, ch := range mo.subs {
		select {
		case ch <- m:
		default: // drop on backpressure
		}
	}
	mo.mu.Unlock()
}

// Subscribe registers a live listener. The returned cancel func must be
// called to release it.
func (mo *Monitor) Subscribe(buffer int) (<-chan MonitorMessage, func()) {
	if buffer <= 0 {
		buffer = 128
	}
	ch := make(chan MonitorMessage, buffer)
	mo.mu.Lock()
	id := mo.nextID
	mo.nextID++
	mo.subs[id] = ch
	mo.mu.Unlock()
	return ch, func() {
		mo.mu.Lock()
		if _, ok := mo.subs[id]; ok {
			delete(mo.subs, id)
			close(ch)
		}
		mo.mu.Unlock()
	}
}

// monitorHook forwards every publish under carvilon/ to the Monitor with
// its full (bounded) payload. It is wired on every broker start next to
// the console + authz hooks, so it is re-established across a
// reconfigure. Topics outside carvilon/ (including $SYS) are ignored -
// the page only watches the device tree.
type monitorHook struct {
	mqtt.HookBase
	monitor *Monitor
}

func (h *monitorHook) ID() string { return "carvilon-mqtt-monitor" }

func (h *monitorHook) Provides(b byte) bool { return b == mqtt.OnPublished }

func (h *monitorHook) OnPublished(cl *mqtt.Client, pk packets.Packet) {
	if !strings.HasPrefix(pk.TopicName, "carvilon/") {
		return
	}
	h.monitor.publish(monitorMessageFromPacket(pk))
}

// monitorMessageFromPacket renders a packet into a MonitorMessage with a
// printable, bounded payload. Non-UTF-8 payloads are flagged binary and
// summarised by length; an over-long payload is truncated.
func monitorMessageFromPacket(pk packets.Packet) MonitorMessage {
	m := MonitorMessage{
		Topic:  pk.TopicName,
		Retain: pk.FixedHeader.Retain,
		Time:   packetTimeMillis(pk),
	}
	p := pk.Payload
	if len(p) > maxMonitorPayload {
		p = p[:maxMonitorPayload]
		m.Truncated = true
	}
	switch {
	case len(p) == 0:
		m.Payload = ""
	case utf8.Valid(p):
		m.Payload = string(p)
	default:
		m.Binary = true
		m.Payload = fmt.Sprintf("<%d bytes binary>", len(pk.Payload))
	}
	return m
}

// packetTimeMillis returns the packet's server-stamped creation time in
// unix milliseconds, falling back to now when the packet carries none
// (Created is unix seconds; retained store packets always set it).
func packetTimeMillis(pk packets.Packet) int64 {
	if pk.Created > 0 {
		return pk.Created * 1000
	}
	return nowMillis()
}

// BrokerStats is a read snapshot of the broker's $SYS counters for the
// health strip. The zero value with ok=false means the broker is down.
type BrokerStats struct {
	ClientsConnected int64 `json:"clientsConnected"`
	Uptime           int64 `json:"uptime"` // seconds
	MessagesReceived int64 `json:"messagesReceived"`
	MessagesSent     int64 `json:"messagesSent"`
	Retained         int64 `json:"retained"`
	Subscriptions    int64 `json:"subscriptions"`
	BytesReceived    int64 `json:"bytesReceived"`
	BytesSent        int64 `json:"bytesSent"`
}

// Stats returns the broker's live $SYS counters, or ok=false when it is
// not running. Uptime is taken from the maintained counter and falls
// back to (now - Started) so it is never a stale zero.
func (m *Manager) Stats() (BrokerStats, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.running || m.srv == nil || m.srv.Info == nil {
		return BrokerStats{}, false
	}
	i := m.srv.Info.Clone()
	uptime := i.Uptime
	if uptime <= 0 && i.Started > 0 {
		if d := time.Now().Unix() - i.Started; d > 0 {
			uptime = d
		}
	}
	return BrokerStats{
		ClientsConnected: i.ClientsConnected,
		Uptime:           uptime,
		MessagesReceived: i.MessagesReceived,
		MessagesSent:     i.MessagesSent,
		Retained:         i.Retained,
		Subscriptions:    i.Subscriptions,
		BytesReceived:    i.BytesReceived,
		BytesSent:        i.BytesSent,
	}, true
}

// ClientInfo identifies one device client currently connected to the
// broker. The inline (in-process engine) client is never included.
type ClientInfo struct {
	ClientID string `json:"clientId"`
	Username string `json:"username"`
	Remote   string `json:"remote"`
	Listener string `json:"listener"`
}

// Clients returns the device clients currently connected to the broker
// (open sessions only; the inline client is skipped). Nil when the
// broker is not running.
func (m *Manager) Clients() []ClientInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.running || m.srv == nil {
		return nil
	}
	all := m.srv.Clients.GetAll()
	out := make([]ClientInfo, 0, len(all))
	for _, cl := range all {
		if cl.Net.Inline || cl.Closed() {
			continue
		}
		out = append(out, ClientInfo{
			ClientID: cl.ID,
			Username: string(cl.Properties.Username),
			Remote:   cl.Net.Remote,
			Listener: cl.Net.Listener,
		})
	}
	return out
}

// Retained returns the retained messages matching filter (e.g.
// "carvilon/#" or one device's "carvilon/<user>/#") as the same shape
// the live stream uses, so a page can seed its topic tree then apply
// deltas uniformly. Nil when the broker is not running.
func (m *Manager) Retained(filter string) []MonitorMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.running || m.srv == nil {
		return nil
	}
	pkts := m.srv.Topics.Messages(filter)
	out := make([]MonitorMessage, 0, len(pkts))
	for _, pk := range pkts {
		out = append(out, monitorMessageFromPacket(pk))
	}
	return out
}

// Monitor exposes the live-publish fan-out hub for the device-monitoring
// SSE handler.
func (m *Manager) Monitor() *Monitor { return m.monitor }
