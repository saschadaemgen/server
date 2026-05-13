// Package eventbus is a tiny in-process pub/sub for pushing
// per-viewer events (doorbell ring/cancel, config changed,
// token rotated). Saison 13-02-FIX4-d uses it to feed the
// SSE stream of adopted ESP-Viewer; Saison 13-03 will plug
// the same publish-side into the existing /m/-tree SSE for
// web viewers.
//
// Topology:
//
//	Publisher -> Bus.Publish(mac, Event)
//	Bus       -> per-subscriber buffered channel
//	Subscriber -> ranges over <-chan Event, Unsubscribes on exit
//
// The bus is non-blocking on Publish: if a subscriber is too
// slow and its 16-event buffer is full, the event is dropped
// for that subscriber and a counter increments. SSE-clients
// that can't keep up should reconnect.
package eventbus

import (
	"sync"
	"sync/atomic"
)

// SubscriberBuffer is the per-channel buffer size. 16 is the
// same number the mock library uses for doorbell channels.
const SubscriberBuffer = 16

// Event carries one push payload. Type is the SSE event-name
// ("doorbell.ring", "doorbell.cancel", "config.changed",
// "auth.token.rotate"). JSON is the already-serialized data
// payload that goes into the SSE data: line.
type Event struct {
	Type string
	JSON string
}

// Bus is the central dispatcher. Zero value is not usable;
// use New().
type Bus struct {
	mu          sync.RWMutex
	subscribers map[string]map[chan Event]struct{}

	dropped atomic.Int64
}

// New constructs an empty Bus.
func New() *Bus {
	return &Bus{
		subscribers: make(map[string]map[chan Event]struct{}),
	}
}

// Subscribe returns a channel that will receive every event
// published for the given subject (typically a viewer MAC).
// The channel is buffered SubscriberBuffer events deep; if it
// fills up, additional events for that subscriber are dropped
// (logged via DroppedCount).
//
// Callers MUST call Unsubscribe(subject, ch) when finished or
// the bus will leak the channel and keep dropping events into
// it forever.
func (b *Bus) Subscribe(subject string) <-chan Event {
	ch := make(chan Event, SubscriberBuffer)
	b.mu.Lock()
	defer b.mu.Unlock()
	subs, ok := b.subscribers[subject]
	if !ok {
		subs = make(map[chan Event]struct{})
		b.subscribers[subject] = subs
	}
	subs[ch] = struct{}{}
	return ch
}

// Unsubscribe removes the channel from the subject's fan-out
// set and closes it. Safe to call once; calling twice with
// the same channel is a no-op.
func (b *Bus) Unsubscribe(subject string, ch <-chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	subs, ok := b.subscribers[subject]
	if !ok {
		return
	}
	for owned := range subs {
		if (<-chan Event)(owned) == ch {
			delete(subs, owned)
			close(owned)
			break
		}
	}
	if len(subs) == 0 {
		delete(b.subscribers, subject)
	}
}

// Publish fans the event out to every subscriber of subject.
// Non-blocking: a slow subscriber drops the event rather than
// stalling the publisher. Returns the number of subscribers
// the event was attempted to be delivered to.
func (b *Bus) Publish(subject string, ev Event) int {
	b.mu.RLock()
	subs := b.subscribers[subject]
	channels := make([]chan Event, 0, len(subs))
	for ch := range subs {
		channels = append(channels, ch)
	}
	b.mu.RUnlock()
	for _, ch := range channels {
		select {
		case ch <- ev:
		default:
			b.dropped.Add(1)
		}
	}
	return len(channels)
}

// PublishAll fans the event out to every subscriber of every
// listed subject. Convenience for the "answer one ring,
// cancel everywhere else" path.
func (b *Bus) PublishAll(subjects []string, ev Event) int {
	total := 0
	for _, s := range subjects {
		total += b.Publish(s, ev)
	}
	return total
}

// SubscriberCount returns how many subscribers a subject
// currently has. Useful for tests and for logging when an
// event has nowhere to go.
func (b *Bus) SubscriberCount(subject string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers[subject])
}

// DroppedCount is the cumulative number of events dropped
// because a subscriber's channel was full.
func (b *Bus) DroppedCount() int64 {
	return b.dropped.Load()
}
