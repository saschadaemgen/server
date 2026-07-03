// Package logbuf keeps the last N structured log entries of the whole
// server in memory and fans them out to live subscribers, feeding the
// designer's System Log tab. It hooks in as a tee slog.Handler wrapped
// around the existing stdout handler: stdout/journald output stays
// byte-identical (the tee only observes), the ring buffer is additive.
// There is no persistence — journald remains the archive.
package logbuf

import (
	"context"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Entry is one buffered log line as the System Log tab shows it.
type Entry struct {
	Time      int64  `json:"t"`     // unix milliseconds
	Level     string `json:"level"` // DEBUG|INFO|WARN|ERROR (slog names)
	Subsystem string `json:"sys"`   // component attr, else caller package
	Message   string `json:"msg"`   // message plus "k=v" attrs
}

// Buffer is a fan-out hub: the tee handler Publishes entries; the SSE
// handler reads a Backlog snapshot then Subscribes for live entries.
// Mirrors the MQTT console's snapshot+subscribe shape.
type Buffer struct {
	mu      sync.Mutex
	subs    map[int]chan Entry
	nextID  int
	ring    []Entry
	ringCap int
}

// New returns a buffer retaining up to ringCap recent entries.
func New(ringCap int) *Buffer {
	if ringCap <= 0 {
		ringCap = 1000
	}
	return &Buffer{subs: map[int]chan Entry{}, ringCap: ringCap}
}

// Publish records e in the ring and fans it out to live subscribers.
// Slow subscribers drop the entry rather than block the logging path.
func (b *Buffer) Publish(e Entry) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.ring = append(b.ring, e)
	if len(b.ring) > b.ringCap {
		b.ring = b.ring[len(b.ring)-b.ringCap:]
	}
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default: // drop on backpressure
		}
	}
	b.mu.Unlock()
}

// Backlog returns a copy of the retained recent entries.
func (b *Buffer) Backlog() []Entry {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Entry, len(b.ring))
	copy(out, b.ring)
	return out
}

// Subscribe registers a live listener. The returned cancel func must
// be called to release it.
func (b *Buffer) Subscribe(buffer int) (<-chan Entry, func()) {
	if buffer <= 0 {
		buffer = 64
	}
	ch := make(chan Entry, buffer)
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.subs[id] = ch
	b.mu.Unlock()
	cancel := func() {
		b.mu.Lock()
		if _, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(ch)
		}
		b.mu.Unlock()
	}
	return ch, cancel
}

// componentKey is the attr several subsystems already tag their logger
// with (log.With("component", ...)). The tee promotes it to the entry's
// Subsystem column instead of repeating it in the message.
const componentKey = "component"

// teeHandler forwards every record unchanged to the wrapped handler and
// additionally publishes a flattened Entry to the buffer. It is
// immutable after construction, so concurrent Handle calls are safe.
type teeHandler struct {
	inner     slog.Handler
	buf       *Buffer
	component string // from a WithAttrs "component" attr, "" when unset
	attrs     string // preformatted " k=v" pairs accumulated via WithAttrs
	prefix    string // open group prefix ("grp." style) for later attrs
}

// Tee wraps inner so every record it accepts is also retained in buf.
// The inner handler keeps full control of level gating and output; a
// nil buf yields a pass-through.
func Tee(inner slog.Handler, buf *Buffer) slog.Handler {
	return &teeHandler{inner: inner, buf: buf}
}

func (h *teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *teeHandler) Handle(ctx context.Context, r slog.Record) error {
	if h.buf != nil {
		var sb strings.Builder
		sb.WriteString(r.Message)
		sb.WriteString(h.attrs)
		component := h.component
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == componentKey && h.prefix == "" {
				component = a.Value.Resolve().String()
				return true
			}
			appendAttr(&sb, h.prefix, a)
			return true
		})
		sys := component
		if sys == "" {
			sys = subsystemFromPC(r.PC)
		}
		t := r.Time
		if t.IsZero() {
			t = time.Now()
		}
		h.buf.Publish(Entry{
			Time:      t.UnixMilli(),
			Level:     r.Level.String(),
			Subsystem: sys,
			Message:   sb.String(),
		})
	}
	return h.inner.Handle(ctx, r)
}

func (h *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	nh := *h
	nh.inner = h.inner.WithAttrs(attrs)
	var sb strings.Builder
	for _, a := range attrs {
		if a.Key == componentKey && h.prefix == "" {
			nh.component = a.Value.Resolve().String()
			continue
		}
		appendAttr(&sb, h.prefix, a)
	}
	nh.attrs = h.attrs + sb.String()
	return &nh
}

func (h *teeHandler) WithGroup(name string) slog.Handler {
	nh := *h
	nh.inner = h.inner.WithGroup(name)
	if name != "" {
		nh.prefix = h.prefix + name + "."
	}
	return &nh
}

// appendAttr renders one attr as " key=value", expanding group attrs
// with a dotted prefix (the text handler's convention).
func appendAttr(sb *strings.Builder, prefix string, a slog.Attr) {
	if a.Equal(slog.Attr{}) {
		return
	}
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p = prefix + a.Key + "."
		}
		for _, ga := range v.Group() {
			appendAttr(sb, p, ga)
		}
		return
	}
	sb.WriteByte(' ')
	sb.WriteString(prefix)
	sb.WriteString(a.Key)
	sb.WriteByte('=')
	sb.WriteString(v.String())
}

// subsystemFromPC derives a fallback subsystem from the log call site:
// the last path element of the calling package ("httpserver",
// "mqttbroker", "main"). "" when the PC cannot be resolved.
func subsystemFromPC(pc uintptr) string {
	if pc == 0 {
		return ""
	}
	frames := runtime.CallersFrames([]uintptr{pc})
	f, _ := frames.Next()
	fn := f.Function // e.g. "carvilon.local/server/internal/mqttbroker.(*Manager).start"
	if fn == "" {
		return ""
	}
	if slash := strings.LastIndexByte(fn, '/'); slash >= 0 {
		fn = fn[slash+1:]
	}
	if dot := strings.IndexByte(fn, '.'); dot >= 0 {
		fn = fn[:dot]
	}
	return fn
}
