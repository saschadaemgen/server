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
// Saison 13-01: the hub also writes every start/cancel to the
// door_events table via the doorhistory.Store. Persistence
// happens BEFORE the SSE fan-out so the new event_id can land in
// the start frame. A persistence failure is logged but does NOT
// abort the dispatch (availability beats audit completeness for
// the doorbell live channel; the warn log surfaces the gap).
//
// Saison 13-03: the hub now also publishes every start/cancel
// to the eventbus.Bus (so adopted ESP-Viewers get the same
// doorbell.ring/doorbell.cancel push their web-viewer cousins
// already see) and calls into doorbellcalls.Service to track
// the active-call lifecycle row that the answer/reject/end-call
// endpoints arbitrate against. Both wires are best-effort: a
// nil bus or nil calls service degrades to the prior behavior.
//
// Sends to subscriber channels are non-blocking; a backed-up
// browser is dropped with a warn log and a Stats counter bump
// rather than blocking the manager-side fan-out.
package doorbellhub

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"carvilon.local/mock"
	"carvilon.local/server/internal/doorbellcalls"
	"carvilon.local/server/internal/doorhistory"
	"carvilon.local/server/internal/eventbus"
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
// inside an SSE `data:` line. EventID is set on doorbell_start
// frames (Saison 13-01) so the browser can mark the event as
// read without an extra DB round-trip; doorbell_cancel frames
// leave it zero.
//
// Saison 14-03-FIX03 Sub-2: UnreadCount carries the
// per-mock unread-doorbell count for TypeUnreadCount frames.
// Other event types leave it at the zero value.
type Event struct {
	Type        string `json:"type"`
	EventID     int64  `json:"event_id,omitempty"`
	MockMAC     string `json:"mock_mac"`
	RequestID   string `json:"request_id"`
	DeviceID    string `json:"device_id,omitempty"`
	RoomID      string `json:"room_id,omitempty"`
	CancelToken string `json:"cancel_token,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	UnreadCount int    `json:"unread_count,omitempty"`
}

// Event type names. Browser code listens for these.
//
// Saison 14-XX TypeConfigChanged: signal-only event that a
// per-viewer setting has been mutated server-side (the mieter
// hit /webviewer/settings, the admin hit /a/web-viewers/{mac}/edit,
// the ESP hit /esp/settings, ...). Subscribers refetch whatever
// they care about; the event itself carries no payload beyond the
// type so receivers cannot drift into reading stale fields.
const (
	TypeDoorbellStart  = "doorbell_start"
	TypeDoorbellCancel = "doorbell_cancel"
	TypeUnreadCount    = "unread_count"
	TypeConfigChanged  = "config.changed"
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
	log     *slog.Logger
	src     Source
	history doorhistory.Store
	bus     *eventbus.Bus
	calls   *doorbellcalls.Service

	mu          sync.RWMutex
	subscribers map[string]map[*Subscriber]struct{}

	eventsTotal   atomic.Int64
	eventsDropped atomic.Int64
}

// Options carries the optional Saison 13-03 dependencies. A
// zero Options keeps Saison 12 behavior; Bus enables the
// parallel eventbus push for ESP-Viewers; Calls enables the
// doorbell_calls lifecycle row writes.
type Options struct {
	Bus   *eventbus.Bus
	Calls *doorbellcalls.Service
}

// New constructs a Hub. Pass mockmanager.Manager as the source
// and a doorhistory.Store for persistence; history may be nil
// in narrow test setups (the hub then skips DB writes).
//
// Saison 13-03: NewWithOptions adds the EventBus + DoorbellCalls
// wires. New is preserved as a thin shim so existing callers
// (and the Saison-12 hub_test.go) keep compiling.
func New(src Source, history doorhistory.Store, log *slog.Logger) *Hub {
	return NewWithOptions(src, history, log, Options{})
}

// NewWithOptions builds a Hub with the optional Saison-13-03
// extras populated.
func NewWithOptions(src Source, history doorhistory.Store, log *slog.Logger, opts Options) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{
		log:         log.With("component", "doorbellhub"),
		src:         src,
		history:     history,
		bus:         opts.Bus,
		calls:       opts.Calls,
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
			h.dispatchDoorbell(ctx, ev)
		case ev := <-cancels:
			h.dispatchCancel(ctx, ev)
		}
	}
}

func (h *Hub) dispatchDoorbell(ctx context.Context, ev mock.DoorbellEvent) {
	if ev.MockMAC == "" {
		h.log.Warn("doorbell event without mock_mac, dropping")
		return
	}
	id := h.persistStart(ctx, ev)
	h.startCall(ctx, ev)
	hubEvent := Event{
		Type:        TypeDoorbellStart,
		EventID:     id,
		MockMAC:     ev.MockMAC,
		RequestID:   ev.RequestID,
		DeviceID:    ev.DeviceID,
		RoomID:      ev.RoomID,
		CancelToken: ev.CancelToken,
		CreatedAt:   ev.ReceivedAt.UnixMilli(),
	}
	h.broadcast(ev.MockMAC, hubEvent)
	h.publishToBus(ev.MockMAC, "doorbell.ring", hubEvent)
	// Saison 14-03-FIX03 Sub-2: every new doorbell row also
	// raises the unread count. Broadcast a separate SSE frame so
	// the screensaver badge updates without the browser doing
	// a /webviewer/unread-count round-trip.
	h.BroadcastUnreadCount(ctx, ev.MockMAC)
}

func (h *Hub) dispatchCancel(ctx context.Context, ev mock.DoorbellCancelEvent) {
	if ev.MockMAC == "" {
		h.log.Warn("doorbell cancel event without mock_mac, dropping")
		return
	}
	h.persistCancel(ctx, ev)
	h.endCallTimeout(ctx, ev)
	hubEvent := Event{
		Type:        TypeDoorbellCancel,
		MockMAC:     ev.MockMAC,
		CancelToken: ev.CancelToken,
		CreatedAt:   ev.ReceivedAt.UnixMilli(),
	}
	h.broadcast(ev.MockMAC, hubEvent)
	h.publishToBus(ev.MockMAC, "doorbell.cancel", hubEvent)
}

// startCall best-effort registers the lifecycle row. The
// cancel_token is the natural call event_id (32-char one-shot
// per UDM /remote_view -> /cancel_doorbell match key).
func (h *Hub) startCall(ctx context.Context, ev mock.DoorbellEvent) {
	if h.calls == nil || ev.CancelToken == "" {
		return
	}
	if err := h.calls.Start(ctx, ev.CancelToken, ev.MockMAC, ev.DeviceID); err != nil {
		h.log.Warn("doorbellcalls start failed",
			"mac", ev.MockMAC,
			"event_id", ev.CancelToken,
			"err", err,
		)
	}
}

// endCallTimeout is the doorbellhub-side end-of-life: UDM sent
// /cancel_doorbell_notification (or the inactivity timer fired)
// before any viewer accepted. Idempotent so a real
// rejected/answered_elsewhere/user_ended reason that landed
// first via the HTTP endpoints survives.
func (h *Hub) endCallTimeout(ctx context.Context, ev mock.DoorbellCancelEvent) {
	if h.calls == nil || ev.CancelToken == "" {
		return
	}
	err := h.calls.MarkEnded(ctx, ev.CancelToken, "", doorbellcalls.ReasonTimeout)
	if err != nil && err.Error() != "doorbellcalls: call not found" {
		h.log.Warn("doorbellcalls timeout end failed",
			"mac", ev.MockMAC,
			"event_id", ev.CancelToken,
			"err", err,
		)
	}
}

// publishToBus serializes the doorbell_start/_cancel hub event
// into the JSON shape the ESP firmware reads via SSE (matches
// the briefing TEIL A.3 sample). event_id on the wire is the
// stable cancel_token, not the doorhistory id, because the
// HTTP-answer/reject endpoints look the row up by cancel_token.
func (h *Hub) publishToBus(mockMAC, eventType string, hubEvent Event) {
	if h.bus == nil {
		return
	}
	payload := map[string]any{
		"event_id":     hubEvent.CancelToken,
		"mock_mac":     hubEvent.MockMAC,
		"device_id":    hubEvent.DeviceID,
		"room_id":      hubEvent.RoomID,
		"cancel_token": hubEvent.CancelToken,
		"request_id":   hubEvent.RequestID,
		"created_at":   hubEvent.CreatedAt,
	}
	js, err := json.Marshal(payload)
	if err != nil {
		h.log.Warn("eventbus payload marshal failed", "err", err)
		return
	}
	h.bus.Publish(mockMAC, eventbus.Event{Type: eventType, JSON: string(js)})
}

// persistStart writes the doorbell_start row and returns the new
// id (0 on failure or when history is nil). Failure does not
// abort the SSE dispatch.
func (h *Hub) persistStart(ctx context.Context, ev mock.DoorbellEvent) int64 {
	if h.history == nil {
		return 0
	}
	occurred := ev.ReceivedAt
	if occurred.IsZero() {
		occurred = time.Now()
	}
	id, err := h.history.Insert(ctx, doorhistory.Event{
		MockMAC:     ev.MockMAC,
		EventType:   doorhistory.TypeDoorbellStart,
		IntercomMAC: ev.DeviceID,
		OccurredAt:  occurred,
		CancelToken: ev.CancelToken,
		RoomID:      ev.RoomID,
	}, ev.RawBody)
	if err != nil {
		h.log.Warn("doorhistory insert failed",
			"mac", ev.MockMAC,
			"request_id", ev.RequestID,
			"err", err,
		)
		return 0
	}
	return id
}

// persistCancel marks the matching start row as cancelled. A
// missing row is logged but otherwise harmless; the SSE cancel
// fires regardless.
func (h *Hub) persistCancel(ctx context.Context, ev mock.DoorbellCancelEvent) {
	if h.history == nil {
		return
	}
	occurred := ev.ReceivedAt
	if occurred.IsZero() {
		occurred = time.Now()
	}
	err := h.history.UpdateCancel(ctx, ev.MockMAC, ev.CancelToken, occurred)
	if err != nil {
		h.log.Warn("doorhistory cancel update failed",
			"mac", ev.MockMAC,
			"cancel_token", ev.CancelToken,
			"err", err,
		)
	}
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

// BroadcastConfigChanged fans a TypeConfigChanged event out to
// every subscriber for viewerMAC, on both the SSE side (web
// viewers via /webviewer/events) and the eventbus side (ESP
// viewers via /esp/events). Subscribers refetch their config
// from /esp/config or /webviewer/settings; the SSE/eventbus
// payload itself is empty (`{}`) so the receivers cannot drift
// into reading stale fields.
//
// Saison 14-XX. Filter is per-viewer-MAC so no cross-tenant leak.
func (h *Hub) BroadcastConfigChanged(ctx context.Context, viewerMAC string) {
	if viewerMAC == "" {
		return
	}
	h.broadcast(viewerMAC, Event{
		Type:      TypeConfigChanged,
		MockMAC:   viewerMAC,
		CreatedAt: time.Now().UnixMilli(),
	})
	if h.bus != nil {
		h.bus.Publish(viewerMAC, eventbus.Event{Type: TypeConfigChanged, JSON: "{}"})
	}
}

// BroadcastUnreadCount queries the doorhistory store for the
// current unread-doorbell count for mockMAC and pushes a
// TypeUnreadCount SSE frame to every subscriber. Used by:
//
//   - dispatchDoorbell (count just went up after persistStart)
//   - handler_mieter_history.go after the async MarkRead
//     completes (count just went down to 0)
//
// No-op if history is unwired (test stubs) or the query fails.
// Saison 14-03-FIX03 Sub-2.
func (h *Hub) BroadcastUnreadCount(ctx context.Context, mockMAC string) {
	if h.history == nil || mockMAC == "" {
		return
	}
	n, err := h.history.UnreadCount(ctx, mockMAC)
	if err != nil {
		h.log.Warn("unread count broadcast: query failed",
			"mac", mockMAC, "err", err)
		return
	}
	h.broadcast(mockMAC, Event{
		Type:        TypeUnreadCount,
		MockMAC:     mockMAC,
		UnreadCount: n,
		CreatedAt:   time.Now().UnixMilli(),
	})
}

// Publish lets callers feed events into the hub bypassing the
// source channels. Tests use it directly; saison 14 will wire
// a webhook receiver through the same path. Persistence is the
// caller's job in this path because the payload shape there is
// driven by the webhook envelope, not by the mock DoorbellEvent.
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
