// Package doorbellhub bridges the mockmanager event channels to
// per-tenant SSE subscribers. The hub reads doorbell starts and
// cancels from the manager, resolves the receiving mock viewer
// to its bound ua_user_id, and fans the event out to every
// subscriber registered for that user.
//
// Sends to subscriber channels are non-blocking; a backed-up
// browser is dropped with a warn log and a Stats counter bump
// rather than blocking the manager-side fan-out.
package doorbellhub

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"unifix.local/mock"
)

// Source is the subset of mockmanager.Manager that the hub
// needs. Defined as an interface so tests can inject a fake
// without spinning up the full manager.
type Source interface {
	Events() <-chan mock.DoorbellEvent
	Cancels() <-chan mock.DoorbellCancelEvent
	LookupUserByMAC(ctx context.Context, mac string) (string, error)
}

// Subscriber buffers events destined for one HTTP/SSE
// connection. The channel buffer is small (8) on purpose: a
// stalled browser is dropped, not allowed to back-pressure the
// rest of the platform.
type Subscriber struct {
	UAUserID string
	Events   chan Event
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
	UniqueUserCount int
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

// Subscribe registers a Subscriber for the given ua_user_id.
// The returned cleanup function must be called on disconnect;
// it removes the subscriber and closes its event channel so
// readers can drain via the ok-clause of a channel receive.
func (h *Hub) Subscribe(uaUserID string) (*Subscriber, func()) {
	sub := &Subscriber{
		UAUserID: uaUserID,
		Events:   make(chan Event, subscriberBuffer),
	}
	h.mu.Lock()
	if h.subscribers[uaUserID] == nil {
		h.subscribers[uaUserID] = make(map[*Subscriber]struct{})
	}
	h.subscribers[uaUserID][sub] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			h.mu.Lock()
			if set, ok := h.subscribers[uaUserID]; ok {
				delete(set, sub)
				if len(set) == 0 {
					delete(h.subscribers, uaUserID)
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
			h.dispatchDoorbell(ctx, ev)
		case ev := <-cancels:
			h.dispatchCancel(ctx, ev)
		}
	}
}

func (h *Hub) dispatchDoorbell(ctx context.Context, ev mock.DoorbellEvent) {
	uaUserID, err := h.lookupUser(ctx, ev.MockMAC)
	if err != nil {
		return
	}
	out := Event{
		Type:        TypeDoorbellStart,
		MockMAC:     ev.MockMAC,
		RequestID:   ev.RequestID,
		DeviceID:    ev.DeviceID,
		RoomID:      ev.RoomID,
		CancelToken: ev.CancelToken,
		CreatedAt:   ev.ReceivedAt.UnixMilli(),
	}
	h.broadcast(uaUserID, out)
}

func (h *Hub) dispatchCancel(ctx context.Context, ev mock.DoorbellCancelEvent) {
	uaUserID, err := h.lookupUser(ctx, ev.MockMAC)
	if err != nil {
		return
	}
	out := Event{
		Type:        TypeDoorbellCancel,
		MockMAC:     ev.MockMAC,
		CancelToken: ev.CancelToken,
		CreatedAt:   ev.ReceivedAt.UnixMilli(),
	}
	h.broadcast(uaUserID, out)
}

// lookupUser maps a mock-MAC to its bound ua_user_id, logging
// the two interesting non-routable cases (no binding, lookup
// error) and returning a sentinel error so the caller can
// short-circuit.
func (h *Hub) lookupUser(ctx context.Context, mac string) (string, error) {
	uaUserID, err := h.src.LookupUserByMAC(ctx, mac)
	if err != nil {
		h.log.Info("doorbell from unassigned mock",
			"mac", mac, "err", err.Error())
		return "", err
	}
	if uaUserID == "" {
		h.log.Info("doorbell from mock without ua_user_id binding",
			"mac", mac)
		return "", errors.New("doorbellhub: no binding")
	}
	return uaUserID, nil
}

func (h *Hub) broadcast(uaUserID string, ev Event) {
	h.eventsTotal.Add(1)
	h.mu.RLock()
	defer h.mu.RUnlock()
	for sub := range h.subscribers[uaUserID] {
		select {
		case sub.Events <- ev:
		default:
			h.eventsDropped.Add(1)
			h.log.Warn("subscriber channel full, dropping event",
				"user_prefix", userPrefix(uaUserID),
				"type", ev.Type,
			)
		}
	}
}

// Publish lets callers feed events into the hub bypassing the
// source channels. Saison 12 uses it from tests; saison 13+
// could wire a webhook receiver through the same path.
func (h *Hub) Publish(uaUserID string, ev Event) {
	if uaUserID == "" {
		return
	}
	h.eventsTotal.Add(1)
	h.mu.RLock()
	defer h.mu.RUnlock()
	for sub := range h.subscribers[uaUserID] {
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
		UniqueUserCount: len(h.subscribers),
		EventsTotal:     h.eventsTotal.Load(),
		EventsDropped:   h.eventsDropped.Load(),
	}
}

// userPrefix returns the first 8 characters of a ua_user_id so
// logs stay PII-light per the saison-12 logging convention.
func userPrefix(uaUserID string) string {
	if len(uaUserID) > 8 {
		return uaUserID[:8]
	}
	return uaUserID
}

// hubClock is unused but exists so tests can prove the Run loop
// is the only thing that blocks shutdown.
var _ = time.Now
