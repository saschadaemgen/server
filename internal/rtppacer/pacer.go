// Package rtppacer is a leaky token-bucket RTP send pacer for the WHEP egress,
// wrapped as a pion interceptor.
//
// # Why
//
// On a real 4G leg the residual problem after RTX + FlexFEC is a periodic HARD
// burst: a clump of our packets is sent in one slot, a fixed-duration radio
// loss window catches several of them at once (measured d=6 lost together),
// which exceeds the FlexFEC 10:5 budget (covers 5) so the sixth slips through
// and only RTX recovers it ~1 RTT later, during which the jitter buffer drains
// and render dips.
//
// Pacing spreads our packets over time, so at any instant FEWER of our packets
// are in the air. A loss window of fixed duration then catches FEWER of them
// (d=6 -> d<=5), which the EXISTING FlexFEC 10:5 covers proactively (no RTX,
// no dip). The pacer does not change FEC or the encoder; it only re-times the
// egress.
//
// # Placement
//
// Added to the egress interceptor registry FIRST, which makes it INNERMOST
// (closest to the transport) in pion's chain: track -> NACK responder ->
// FlexFEC -> pacer -> transport. So it paces the FINAL media AND FEC packets.
// RTX retransmissions originate at the responder (outermost) and also pass
// through the pacer, so they are lightly paced too - an accepted compromise: a
// single in-chain pacer cannot both pace media+FEC and bypass RTX (responder
// and FlexFEC sit at opposite ends, and a retransmit is indistinguishable from
// an original at the interceptor layer). At the configured rate with only a
// handful of retransmits this is sub-frame and negligible.
//
// # Design vs gcc.LeakyBucketPacer
//
// We deliberately do NOT use gcc.LeakyBucketPacer: it routes by header.SSRC and
// DROPS packets whose SSRC was never registered (it would silently drop FEC),
// has a hidden f=1.5 rate multiplier, and logs per packet. This pacer instead
// captures each stream's downstream writer at bind time and stores it on every
// queued packet, so media, FEC, and RTX all reach the correct sink with NO
// SSRC routing and NO drop. RTCP is untouched (only BindLocalStream is
// overridden; everything else is interceptor.NoOp).
//
// One Pacer instance per PeerConnection (built by Factory.NewInterceptor), with
// its own drain goroutine, closed on PC teardown.
package rtppacer

import (
	"container/list"
	"io"
	"log"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

const (
	// DefaultTick is the drain interval. 5 ms is fine-grained enough to spread
	// a keyframe smoothly without a busy loop.
	DefaultTick = 5 * time.Millisecond

	// burstSeconds caps accrued tokens at rate*burstSeconds. Sized so a normal
	// per-frame packet group passes immediately using tokens accrued in the gap
	// between frames, while a much larger keyframe burst exceeds the cap and is
	// metered out at the target rate (spread over ~1-2 frame intervals). ~25 ms
	// sits above one P-frame group and below a keyframe at our rates.
	burstSeconds = 0.025
)

// queued is one packet awaiting its pacing slot. The downstream writer is
// captured per packet, so there is no SSRC routing (and thus no drop): media,
// FEC, and RTX each carry the correct sink.
type queued struct {
	header  rtp.Header
	payload []byte
	attrs   interceptor.Attributes
	writer  interceptor.RTPWriter
}

// Pacer is a pion interceptor that paces outgoing RTP via a leaky token bucket.
type Pacer struct {
	interceptor.NoOp // RTCP read/write, remote stream, unbind: all no-ops

	bytesPerSec float64
	maxTokens   float64
	tick        time.Duration
	logger      *log.Logger

	mu     sync.Mutex
	queue  *list.List
	closed bool

	done      chan struct{}
	closeOnce sync.Once
}

func newPacer(targetBitrate int, tick time.Duration, logger *log.Logger) *Pacer {
	if tick <= 0 {
		tick = DefaultTick
	}
	if logger == nil {
		logger = log.Default()
	}
	bps := float64(targetBitrate) / 8.0
	p := &Pacer{
		bytesPerSec: bps,
		maxTokens:   bps * burstSeconds,
		tick:        tick,
		logger:      logger,
		queue:       list.New(),
		done:        make(chan struct{}),
	}
	go p.run()

	return p
}

// BindLocalStream wraps the outgoing RTP writer. Every packet written here is
// copied and enqueued with THIS stream's downstream writer captured, then
// released by the drain loop. Unknown SSRCs are never dropped.
func (p *Pacer) BindLocalStream(_ *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attrs interceptor.Attributes) (int, error) {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()

			return writer.Write(header, payload, attrs) // teardown: straight through
		}
		buf := make([]byte, len(payload))
		copy(buf, payload)
		p.queue.PushBack(&queued{
			header:  header.Clone(),
			payload: buf,
			attrs:   attrs,
			writer:  writer,
		})
		p.mu.Unlock()

		// Report the bytes as accepted; they leave on a later tick.
		return header.MarshalSize() + len(payload), nil
	})
}

// run is the drain loop: each tick it refills the token bucket (capped at
// maxTokens) and releases queued packets while tokens remain. A packet larger
// than the remaining tokens is still sent once tokens are > 0 (tokens go
// negative, then refill), so a big packet never stalls forever.
func (p *Pacer) run() {
	ticker := time.NewTicker(p.tick)
	defer ticker.Stop()

	tokens := p.maxTokens // start full: an early small burst is not throttled
	last := time.Now()
	for {
		select {
		case <-p.done:
			p.flush()

			return
		case now := <-ticker.C:
			tokens += now.Sub(last).Seconds() * p.bytesPerSec
			if tokens > p.maxTokens {
				tokens = p.maxTokens
			}
			last = now
			for tokens > 0 {
				it := p.pop()
				if it == nil {
					break
				}
				n, err := it.writer.Write(&it.header, it.payload, it.attrs)
				if err != nil {
					p.logger.Printf("rtppacer: write: %v", err)
				}
				tokens -= float64(n)
			}
		}
	}
}

// pop removes and returns the front queued packet, or nil if empty.
func (p *Pacer) pop() *queued {
	p.mu.Lock()
	defer p.mu.Unlock()
	front := p.queue.Front()
	if front == nil {
		return nil
	}

	return p.queue.Remove(front).(*queued)
}

// flush releases everything still queued, ignoring the rate, so no packet is
// lost on close/teardown.
func (p *Pacer) flush() {
	for {
		it := p.pop()
		if it == nil {
			return
		}
		_, _ = it.writer.Write(&it.header, it.payload, it.attrs)
	}
}

// Close stops the pacer and flushes the queue. Idempotent. Implements
// interceptor.Interceptor.Close (called by the chain on PC teardown).
func (p *Pacer) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		close(p.done)
	})

	return nil
}

// Factory builds one Pacer per PeerConnection. Implements interceptor.Factory,
// so it is added to the egress interceptor.Registry like the NACK responder.
type Factory struct {
	targetBitrate int
	tick          time.Duration
	logger        *log.Logger
}

// NewFactory returns a pacer factory that paces each PeerConnection's egress at
// targetBitrate bits/sec. Pick ~2-3x the media bitrate: high enough that steady
// P-frames pass via accrued burst tokens, low enough that a keyframe is metered
// over ~1-2 frame intervals. logger nil -> the default logger.
func NewFactory(targetBitrate int, logger *log.Logger) *Factory {
	return &Factory{targetBitrate: targetBitrate, tick: DefaultTick, logger: logger}
}

// NewInterceptor implements interceptor.Factory: a fresh Pacer per PC.
func (f *Factory) NewInterceptor(_ string) (interceptor.Interceptor, error) {
	return newPacer(f.targetBitrate, f.tick, f.logger), nil
}

// compile-time guards.
var (
	_ interceptor.Interceptor = (*Pacer)(nil)
	_ interceptor.Factory     = (*Factory)(nil)
	_ io.Closer               = (*Pacer)(nil)
)
