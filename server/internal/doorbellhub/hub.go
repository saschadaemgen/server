// Package doorbellhub bridges the mockmanager event channels to
// per-mock SSE subscribers. The hub reads doorbell starts and
// cancels from the manager and fans them out to every subscriber
// registered for the receiving mock's MAC.
//
// Saison 12-06 refactor: subscribers are indexed by mock_mac
// (not ua_user_id). The Source interface no longer carries a
// LookupUserByMAC indirection; the routing key is right there
// on the incoming event.
//
// Sends to subscriber channels are non-blocking; a backed-up
// browser is dropped with a warn log and a Stats counter bump
// rather than blocking the manager-side fan-out.
package doorbellhub

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"unifix.local/mock"
)

// Source is the subset of mockmanager.Manager that the hub
// needs. Defined as an interface so tests can inject a fake
// without spinning up the full manager.
type Source interface {
	Events() <-chan mock.DoorbellEvent
	Cancels() <-chan mock.DoorbellCancelEvent
}

// Subscriber buffers events destined for one HTTP/SSE
// connection. The channel buffer is small (8) on purpose: a
// stalled browser is dropped, not allowed to back-pressure the
// rest of the platform.
type Subscriber struct {
	MockMAC string
	Events  chan Event
}

const subscriberBuffer = 8

// Event is the wire shape sent to the browser. JSON-encoded
// inside an SSE `data:` line.
type Event struct {
	Type        string `json:"type"`
	MockMAC     string `json:"mock_mac"`
	RequestID   string `json:"request_id"`
	DeviceID    string `json:"device_id,omitempty"`
	RoomID      string `json:"room_id,omitempty"`
	CancelToken string `json:"cancel_token,omitempty"`
	CreatedAt   int64  `json:"created_at"`
}

// Event type names. Browser code listens for these.
const (
	TypeDoorbellStart  = "doorbell_start"
	TypeDoorbellCancel = "doorbell_cancel"
)

// Stats is a debugging snapshot of the hub state.
type Stats struct {
	SubscriberCount int
	UniqueMockCount int
	EventsTotal     int64
	EventsDropped   int64
}

// Hub is the singleton fan-out. Construct with New, start with
// Run, subscribe with Subscribe.
type Hub struct {
	log *slog.Logger
	src Source

	mu          sync.RWMutex
	subscribers map[string]map[*Subscriber]struct{}

	eventsTotal   atomic.Int64
	eventsDropped atomic.Int64
}

// New constructs a Hub. Pass mockmanager.Manager as the source.
func New(src Source, log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{
		log:         log.With("component", "doorbellhub"),
		src:         src,
		subscribers: make(map[string]map[*Subscriber]struct{}),
	}
}

// Subscribe registers a Subscriber for the given mock-MAC.
// The returned cleanup function must be called on disconnect;
// it removes the subscriber and closes its event channel so
// readers can drain via the ok-clause of a channel receive.
func (h *Hub) Subscribe(mockMAC string) (*Subscriber, func()) {
	sub := &Subscriber{
		MockMAC: mockMAC,
		Events:  make(chan Event, subscriberBuffer),
	}
	h.mu.Lock()
	if h.subscribers[mockMAC] == nil {
		h.subscribers[mockMAC] = make(map[*Subscriber]struct{})
	}
	h.subscribers[mockMAC][sub] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			h.mu.Lock()
			if set, ok := h.subscribers[mockMAC]; ok {
				delete(set, sub)
				if len(set) == 0 {
					delete(h.subscribers, mockMAC)
				}
			}
			close(sub.Events)
			h.mu.Unlock()
		})
	}
	return sub, cleanup
}

// Run pumps the source channels into the hub fan-out. Returns
// ctx.Err() on shutdown.
func (h *Hub) Run(ctx context.Context) error {
	events := h.src.Events()
	cancels := h.src.Cancels()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-events:
			h.dispatchDoorbell(ev)
		case ev := <-cancels:
			h.dispatchCancel(ev)
		}
	}
}

func (h *Hub) dispatchDoorbell(ev mock.DoorbellEvent) {
	if ev.MockMAC == "" {
		h.log.Warn("doorbell event without mock_mac, dropping")
		return
	}
	h.broadcast(ev.MockMAC, Event{
		Type:        TypeDoorbellStart,
		MockMAC:     ev.MockMAC,
		RequestID:   ev.RequestID,
		DeviceID:    ev.DeviceID,
		RoomID:      ev.RoomID,
		CancelToken: ev.CancelToken,
		CreatedAt:   ev.ReceivedAt.UnixMilli(),
	})
}

func (h *Hub) dispatchCancel(ev mock.DoorbellCancelEvent) {
	if ev.MockMAC == "" {
		h.log.Warn("doorbell cancel event without mock_mac, dropping")
		return
	}
	h.broadcast(ev.MockMAC, Event{
		Type:        TypeDoorbellCancel,
		MockMAC:     ev.MockMAC,
		CancelToken: ev.CancelToken,
		CreatedAt:   ev.ReceivedAt.UnixMilli(),
	})
}

func (h *Hub) broadcast(mockMAC string, ev Event) {
	h.eventsTotal.Add(1)
	h.mu.RLock()
	defer h.mu.RUnlock()
	subs := h.subscribers[mockMAC]
	if len(subs) == 0 {
		h.log.Info("doorbell with no subscribers", "mac", mockMAC, "type", ev.Type)
		return
	}
	for sub := range subs {
		select {
		case sub.Events <- ev:
		default:
			h.eventsDropped.Add(1)
			h.log.Warn("subscriber channel full, dropping event",
				"mac", mockMAC,
				"type", ev.Type,
			)
		}
	}
}

// Publish lets callers feed events into the hub bypassing the
// source channels. Tests use it directly; saison 13+ could wire
// a webhook receiver through the same path.
func (h *Hub) Publish(mockMAC string, ev Event) {
	if mockMAC == "" {
		return
	}
	h.eventsTotal.Add(1)
	h.mu.RLock()
	defer h.mu.RUnlock()
	for sub := range h.subscribers[mockMAC] {
		select {
		case sub.Events <- ev:
		default:
			h.eventsDropped.Add(1)
		}
	}
}

// Stats returns a snapshot of the hub state. Cheap; safe to
// call from /metrics or admin pages.
func (h *Hub) Stats() Stats {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var total int
	for _, set := range h.subscribers {
		total += len(set)
	}
	return Stats{
		SubscriberCount: total,
		UniqueMockCount: len(h.subscribers),
		EventsTotal:     h.eventsTotal.Load(),
		EventsDropped:   h.eventsDropped.Load(),
	}
}
