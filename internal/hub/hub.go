// Package hub implements the CARVILON Fan-Out bus: one camera pull, N
// browser viewers.
//
// Architectural shape (S2-01):
//
//	source.VideoSource ── Frames() ──► Hub.run goroutine ──► Subscriber 1
//	                                                     ├──► Subscriber 2
//	                                                     └──► Subscriber N
//
// All state lives inside a single [Hub.run] goroutine driven by control
// channels — no mutex on the subscriber set, no concurrent map access.
// This trades a little throughput (Subscribe/Unsubscribe serialize against
// frame distribution) for a much simpler correctness story: a single
// goroutine owns the source pointer, the subscriber map, and the cached
// IDR; everything else talks to it via channels.
//
// Drop-statt-buffer (the go2rtc lesson):
//   - Each Subscriber has a small bounded channel.
//   - Distribution is non-blocking per subscriber.
//   - A slow subscriber gets dropped frames; the fast ones are unaffected.
//   - Drops are counted into a per-subscriber [droplog.Counter] so the
//     log shows "subscriber N: dropped K" at most once per second.
//
// Source lifecycle:
//   - First Subscribe → factory builds a fresh source, hub calls Start.
//   - Every later Subscribe joins the same running source.
//   - Last Unsubscribe → hub calls Close on the source; the next
//     Subscribe will rebuild via the factory.
//
// Pre-feed for new subscribers:
//   - The hub keeps a copy of the most recent IDR-containing AU. When a
//     new subscriber joins, that AU is enqueued on its channel before
//     any new frames flow, so the browser decoder can lock on immediately
//     instead of waiting until the next keyframe arrives over the wire.
//   - For sources where every IDR-AU already carries SPS/PPS (as our
//     [unifi.Source] does — see S1-05), this is sufficient bootstrap.
package hub

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"carvilon.local/stream/internal/droplog"
	"carvilon.local/stream/internal/source"
)

// SourceFactory constructs a fresh, un-Started [source.VideoSource]. The
// hub calls this once at first Subscribe, then again after every
// down-to-zero-subscribers cycle. Returning an error fails the Subscribe
// call that triggered the build.
type SourceFactory func() (source.VideoSource, error)

// Options configures a [Hub].
type Options struct {
	// Logger receives diagnostic output. If nil, the default logger.
	Logger *log.Logger

	// SubscriberBuffer is the channel depth of every Subscriber.Frames().
	// Default: 30 (≈1 s of buffer at 30 fps). Tiny enough that a slow
	// consumer gets dropped quickly rather than accumulating latency.
	SubscriberBuffer int
}

const defaultSubscriberBuffer = 30

// Hub is the Fan-Out bus. Constructed with [New], shut down with [Close].
type Hub struct {
	factory SourceFactory
	logger  *log.Logger
	bufSize int

	subCh   chan subscribeReq
	unsubCh chan uint64

	ctx     context.Context
	cancel  context.CancelFunc
	runDone chan struct{}
}

// Subscriber represents one connected viewer. Obtain one from
// [Hub.Subscribe]; release with [Subscriber.Close] when the viewer
// disconnects. Reading from [Subscriber.Frames] yields access units in
// arrival order, with slow-consumer drops counted into the per-subscriber
// log. The channel is closed exactly once when the subscriber is removed
// (by Close, by source end, or by hub shutdown).
type Subscriber struct {
	id     uint64
	frames chan source.AccessUnit
	drops  *droplog.Counter
	hub    *Hub
	once   sync.Once
}

// ID returns the subscriber's unique id (sequential per hub). Useful for
// log correlation.
func (s *Subscriber) ID() uint64 { return s.id }

// Frames returns the read-only stream of access units for this
// subscriber. The channel is closed when the subscriber is removed —
// callers should `for au := range s.Frames() { … }` and exit cleanly.
func (s *Subscriber) Frames() <-chan source.AccessUnit { return s.frames }

// Close removes the subscriber from the hub. Idempotent. Safe to call
// from any goroutine. After Close returns, no more frames will be
// enqueued; the channel is closed by the hub's run goroutine.
func (s *Subscriber) Close() {
	s.once.Do(func() {
		select {
		case s.hub.unsubCh <- s.id:
		case <-s.hub.ctx.Done():
			// Hub is shutting down; it will close all channels itself.
		}
	})
}

// subscribeReq is the message subscribers send to the hub's run goroutine.
type subscribeReq struct {
	resp chan subscribeResp
}

type subscribeResp struct {
	sub *Subscriber
	err error
}

// New constructs a Hub. The factory is invoked lazily; no source is built
// until the first Subscribe call. The hub starts its own goroutine; call
// [Close] to tear it down.
func New(factory SourceFactory, opts Options) *Hub {
	if factory == nil {
		panic("hub: factory must not be nil")
	}
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	if opts.SubscriberBuffer <= 0 {
		opts.SubscriberBuffer = defaultSubscriberBuffer
	}

	ctx, cancel := context.WithCancel(context.Background())
	h := &Hub{
		factory: factory,
		logger:  opts.Logger,
		bufSize: opts.SubscriberBuffer,
		subCh:   make(chan subscribeReq),
		unsubCh: make(chan uint64),
		ctx:     ctx,
		cancel:  cancel,
		runDone: make(chan struct{}),
	}
	go h.run()
	return h
}

// Subscribe registers a new viewer. The returned Subscriber owns a
// freshly-allocated frames channel; the caller should consume it until
// the channel is closed and call [Subscriber.Close] when the viewer
// disconnects.
//
// When this is the first subscriber on an idle hub, Subscribe blocks
// while the source's Start call runs — that includes the Protect-API
// roundtrip and the RTSP setup, typically 1–3 s against UniFi.
//
// Returns an error if the source factory fails or the underlying
// source's Start returns an error.
func (h *Hub) Subscribe() (*Subscriber, error) {
	resp := make(chan subscribeResp, 1)
	select {
	case h.subCh <- subscribeReq{resp: resp}:
	case <-h.ctx.Done():
		return nil, errors.New("hub: closed")
	}
	r := <-resp
	return r.sub, r.err
}

// Close shuts the hub down. All subscriber channels are closed; the
// source (if running) is closed. Idempotent.
func (h *Hub) Close() error {
	h.cancel()
	<-h.runDone
	return nil
}

// run is the hub's serialisation point. It owns the source pointer, the
// subscriber map, the IDR cache, and the id counter — no other code
// touches them. All inputs arrive on channels.
func (h *Hub) run() {
	defer close(h.runDone)

	subscribers := make(map[uint64]*Subscriber)
	var (
		src       source.VideoSource
		srcFrames <-chan source.AccessUnit
		lastIDR   *source.AccessUnit
		nextID    uint64
	)

	// closeAllSubs closes every subscriber's frames channel. Called when
	// the source ends (unexpected or graceful) and on hub shutdown.
	closeAllSubs := func() {
		for id, sub := range subscribers {
			close(sub.frames)
			delete(subscribers, id)
		}
	}

	// stopSource closes the source and clears the local state.
	stopSource := func(reason string) {
		if src == nil {
			return
		}
		_ = src.Close()
		src = nil
		srcFrames = nil
		lastIDR = nil
		h.logger.Printf("hub: source stopped (%s)", reason)
	}

	for {
		select {
		case <-h.ctx.Done():
			stopSource("hub closing")
			closeAllSubs()
			return

		case req := <-h.subCh:
			// If the bus is idle, build and start a fresh source.
			if src == nil {
				newSrc, err := h.factory()
				if err != nil {
					req.resp <- subscribeResp{err: fmt.Errorf("hub: source factory: %w", err)}
					continue
				}
				if err := newSrc.Start(h.ctx); err != nil {
					_ = newSrc.Close()
					req.resp <- subscribeResp{err: fmt.Errorf("hub: source start: %w", err)}
					continue
				}
				src = newSrc
				srcFrames = src.Frames()
				h.logger.Printf("hub: source started for new subscriber")
			}

			nextID++
			sub := &Subscriber{
				id:     nextID,
				frames: make(chan source.AccessUnit, h.bufSize),
				drops:  &droplog.Counter{Logger: h.logger, Label: fmt.Sprintf("hub: subscriber %d", nextID)},
				hub:    h,
			}
			subscribers[nextID] = sub

			// Pre-feed the cached IDR if we have one. The channel is
			// empty (just allocated), so the non-blocking send always
			// succeeds. New viewers start decoding from a keyframe
			// without waiting for the next one over the wire.
			if lastIDR != nil {
				select {
				case sub.frames <- *lastIDR:
				default: // unreachable for a fresh channel, but defensive
				}
			}

			h.logger.Printf("hub: subscriber %d joined (total=%d)", sub.id, len(subscribers))
			req.resp <- subscribeResp{sub: sub}

		case id := <-h.unsubCh:
			sub, ok := subscribers[id]
			if !ok {
				continue
			}
			delete(subscribers, id)
			close(sub.frames)
			h.logger.Printf("hub: subscriber %d left (total=%d)", id, len(subscribers))

			if len(subscribers) == 0 {
				stopSource("last subscriber left")
			}

		case au, ok := <-srcFrames:
			if !ok {
				h.logger.Printf("hub: source frames channel closed (upstream end)")
				stopSource("upstream end")
				closeAllSubs()
				continue
			}

			// Cache the most recent IDR so the next new subscriber can
			// start decoding immediately. We hold a value copy of the
			// AccessUnit; the NAL byte slices remain shared (read-only
			// downstream).
			if au.IsKeyframe {
				idr := au
				lastIDR = &idr
			}

			// Non-blocking distribution. A slow subscriber loses this
			// frame; the others are unaffected, the source is unaffected.
			for _, sub := range subscribers {
				select {
				case sub.frames <- au:
				default:
					sub.drops.Record(errors.New("frames channel full"))
				}
			}
		}
	}
}
