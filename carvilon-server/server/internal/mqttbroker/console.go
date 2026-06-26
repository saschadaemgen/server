package mqttbroker

import (
	"sync"
	"time"
	"unicode/utf8"
)

// maxPreview caps how many payload bytes the console surfaces, so a
// large publish does not balloon the SSE stream or the ring buffer.
const maxPreview = 256

// Event is one line of broker activity shown in the live MQTT
// console. It carries no credentials; payloads are truncated to a
// short printable preview.
type Event struct {
	Time   int64  `json:"t"`             // unix milliseconds
	Kind   string `json:"kind"`          // connect|disconnect|publish|subscribe
	Client string `json:"client"`        // mqtt client id
	User   string `json:"user"`          // device username (auth identity)
	Remote string `json:"remote,omitempty"`
	Topic  string `json:"topic,omitempty"`
	QoS    byte   `json:"qos,omitempty"`
	Size   int    `json:"size,omitempty"`    // payload length in bytes
	Detail string `json:"detail,omitempty"`  // preview / reason / qos summary
}

// Console is a fan-out hub: the broker's hooks Publish events; the
// SSE handler reads a backlog snapshot then Subscribes for live
// events. Mirrors the engine monitor's snapshot+subscribe shape.
type Console struct {
	mu      sync.Mutex
	subs    map[int]chan Event
	nextID  int
	ring    []Event
	ringCap int
}

// NewConsole returns a console retaining up to ringCap recent events.
func NewConsole(ringCap int) *Console {
	if ringCap <= 0 {
		ringCap = 200
	}
	return &Console{subs: map[int]chan Event{}, ringCap: ringCap}
}

// Publish records ev in the ring and fans it out to live subscribers.
// Slow subscribers drop the event rather than block the broker.
func (c *Console) Publish(ev Event) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.ring = append(c.ring, ev)
	if len(c.ring) > c.ringCap {
		c.ring = c.ring[len(c.ring)-c.ringCap:]
	}
	for _, ch := range c.subs {
		select {
		case ch <- ev:
		default: // drop on backpressure
		}
	}
	c.mu.Unlock()
}

// Backlog returns a copy of the retained recent events.
func (c *Console) Backlog() []Event {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Event, len(c.ring))
	copy(out, c.ring)
	return out
}

// Subscribe registers a live listener. The returned cancel func must
// be called to release it.
func (c *Console) Subscribe(buffer int) (<-chan Event, func()) {
	if buffer <= 0 {
		buffer = 64
	}
	ch := make(chan Event, buffer)
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	c.subs[id] = ch
	c.mu.Unlock()
	cancel := func() {
		c.mu.Lock()
		if _, ok := c.subs[id]; ok {
			delete(c.subs, id)
			close(ch)
		}
		c.mu.Unlock()
	}
	return ch, cancel
}

func nowMillis() int64 { return time.Now().UnixMilli() }

// preview returns a short, printable, valid-UTF-8 rendering of a
// payload for display. Non-printable bytes are dropped; the result
// is truncated to maxPreview runes.
func preview(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if len(b) > maxPreview*4 {
		b = b[:maxPreview*4]
	}
	out := make([]rune, 0, maxPreview)
	for len(b) > 0 && len(out) < maxPreview {
		r, sz := utf8.DecodeRune(b)
		b = b[sz:]
		if r == utf8.RuneError {
			out = append(out, '.')
			continue
		}
		if r < 0x20 || r == 0x7f {
			r = '.'
		}
		out = append(out, r)
	}
	return string(out)
}
