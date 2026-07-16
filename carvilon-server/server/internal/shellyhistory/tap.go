// Package shellyhistory records a Shelly device's metered values into the
// sensor-history store (H1) by TAPPING THE STREAM THE DEVICES ALREADY
// PUBLISH. Adopted Shellys push their status to CARVILON's embedded broker;
// this package observes those publishes and turns the ones it recognises into
// (device, metric, value) readings. It adds no second poll and opens no
// connection to a device.
//
// It is the STORED half only. The LIVE path - the editor faceplate's values,
// the cockpit readouts - rides the same broker messages independently via the
// mqtt: driver, and is untouched by anything here. The two must never be
// conflated, and recording must never throttle the live path: Handle runs on
// the broker's publish goroutine and does nothing but a bounded copy and a
// non-blocking hand-off to Run's worker, which drops rather than block.
package shellyhistory

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"carvilon.local/server/internal/shellycaps"
	"carvilon.local/server/internal/shellystore"
)

// maxPayload bounds what the tap will copy off the broker path. A Shelly
// status payload is a few hundred bytes; anything far larger is not one, and
// copying it would be work done in the publisher's goroutine.
const maxPayload = 8 << 10

// indexTTL is how long the derived device index is reused before it is rebuilt
// from the store. A newly adopted device therefore starts recording within
// this window without any explicit invalidation.
const indexTTL = 30 * time.Second

// Store is the device-set view the tap needs (the adopted Shellys).
type Store interface {
	ListActive(ctx context.Context) ([]shellystore.Device, error)
}

// Config wires the tap.
type Config struct {
	Store Store
	// Record is sensorhistory.Recorder.Record. It must be non-blocking.
	Record func(deviceID, metric string, value float64, at time.Time)
	Log    *slog.Logger
	// Now is injectable for tests.
	Now func() time.Time
}

// msg is one observed publish, copied off the broker path.
type msg struct {
	topic   string
	payload []byte
	at      time.Time
}

// device is one adopted Shelly, resolved to what the parser needs.
type device struct {
	histID string // the sensor-history key: shellystore.HistoryID(mac)
	gen1   bool
}

// Tap observes broker publishes and records Shelly readings.
type Tap struct {
	store  Store
	record func(string, string, float64, time.Time)
	log    *slog.Logger
	now    func() time.Time

	in chan msg

	mu       sync.RWMutex
	byPrefix map[string]device // "carvilon/shelly-aabb.." | "shellies/shelly-aabb.." -> device
	built    time.Time
}

// New constructs a Tap. Run must be started for it to record.
func New(cfg Config) *Tap {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Tap{
		store:  cfg.Store,
		record: cfg.Record,
		log:    log.With("component", "shellyhistory"),
		now:    now,
		// Buffered so a burst of status publishes is absorbed rather than
		// dropped; full means the worker is starved, and then dropping is
		// exactly right (see Handle).
		in:       make(chan msg, 256),
		byPrefix: map[string]device{},
	}
}

// Handle is the mqttbroker.ReadingTap. It runs SYNCHRONOUSLY on the broker's
// publish path, in the publishing client's goroutine, so it must never block
// and never do real work: it copies the payload (the packet's buffer is not
// ours to keep) and hands off. A full queue drops the reading rather than
// stall delivery to the live subscribers - a missed sample is a gap in a
// chart, a stalled broker is a broken house.
func (t *Tap) Handle(topic string, payload []byte, at time.Time) {
	if t == nil || t.record == nil || len(payload) == 0 || len(payload) > maxPayload {
		return
	}
	// Cheap reject before the copy: only the two Shelly topic roots.
	if !strings.HasPrefix(topic, "carvilon/") && !strings.HasPrefix(topic, "shellies/") {
		return
	}
	cp := make([]byte, len(payload))
	copy(cp, payload)
	select {
	case t.in <- msg{topic: topic, payload: cp, at: at}:
	default:
		// Worker starved; drop.
	}
}

// Run consumes observed publishes until ctx is cancelled.
func (t *Tap) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-t.in:
			t.ingest(ctx, m)
		}
	}
}

// ingest resolves the device and emits its readings.
func (t *Tap) ingest(ctx context.Context, m msg) {
	prefix, rest, ok := splitPrefix(m.topic)
	if !ok {
		return
	}
	d, ok := t.lookup(ctx, prefix)
	if !ok {
		return
	}
	at := m.at
	if at.IsZero() {
		at = t.now()
	}
	for _, r := range parse(rest, m.payload, d.gen1) {
		t.record(d.histID, r.metric, r.value, at)
	}
}

// lookup resolves a topic prefix to an adopted device, rebuilding the index
// when it is stale. A prefix that is not an adopted Shelly's is a normal miss
// (the device trees carry other first-party traffic too).
func (t *Tap) lookup(ctx context.Context, prefix string) (device, bool) {
	t.mu.RLock()
	fresh := t.now().Sub(t.built) < indexTTL
	d, hit := t.byPrefix[prefix]
	t.mu.RUnlock()
	if fresh {
		return d, hit
	}
	t.rebuild(ctx)
	t.mu.RLock()
	defer t.mu.RUnlock()
	d, hit = t.byPrefix[prefix]
	return d, hit
}

// rebuild derives the prefix index from the adopted device set. The prefix
// derivation MIRRORS the catalog's (handler_admin_designer.go): the
// provisioned broker account, else the conventional "shelly-<mac>"; Gen1
// firmware pins its tree under shellies/, Gen2+ under the assigned carvilon/
// prefix. A device with no MAC has no durable history key and is skipped.
func (t *Tap) rebuild(ctx context.Context) {
	if t.store == nil {
		return
	}
	devs, err := t.store.ListActive(ctx)
	if err != nil {
		t.log.Warn("shelly history: list active failed; keeping the previous index", "err", err)
		// Do not clear the index on a transient store error - keep recording.
		t.mu.Lock()
		t.built = t.now()
		t.mu.Unlock()
		return
	}
	idx := make(map[string]device, len(devs))
	for _, d := range devs {
		hist := shellystore.HistoryID(d.MAC)
		if hist == "" {
			continue
		}
		user := d.MQTTUsername
		if user == "" {
			user = hist // the account the provisioner would mint
		}
		gen1 := d.Gen == shellystore.Gen1
		root := "carvilon/"
		if gen1 {
			root = "shellies/"
		}
		idx[root+user] = device{histID: hist, gen1: gen1}
	}
	t.mu.Lock()
	t.byPrefix = idx
	t.built = t.now()
	t.mu.Unlock()
}

// splitPrefix splits "<root>/<account>/<rest...>" into the two-segment device
// prefix and the remainder.
func splitPrefix(topic string) (prefix, rest string, ok bool) {
	parts := strings.SplitN(topic, "/", 3)
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[0] + "/" + parts[1], parts[2], true
}

type reading struct {
	metric string
	value  float64
}

// gen2Switch is the metered part of a Gen2 status/switch:N payload. The field
// names are the device's, and are the same ones the editor faceplate already
// reads off this exact payload - not a guess. Pointers so an absent field is
// distinguishable from a real zero: a device that reports no voltage must
// record no voltage, not 0 V.
type gen2Switch struct {
	APower  *float64 `json:"apower"`
	Voltage *float64 `json:"voltage"`
	Current *float64 `json:"current"`
	Freq    *float64 `json:"freq"`
	Temp    *struct {
		C *float64 `json:"tC"`
	} `json:"temperature"`
}

// gen1Light is the metered part of a Gen1 RGBW2 color|white/N/status payload.
// Its power field is "power", NOT the Gen2 "apower".
type gen1Light struct {
	Power *float64 `json:"power"`
}

// parse turns one device-relative topic + payload into readings. It returns
// nothing for the many topics that carry no metered value (commands, relay
// state, inputs, announcements), which is the common case.
func parse(rest string, payload []byte, gen1 bool) []reading {
	switch {
	case !gen1 && strings.HasPrefix(rest, "status/switch:"):
		ch, ok := atoiSuffix(rest, "status/switch:")
		if !ok {
			return nil
		}
		var s gen2Switch
		if err := json.Unmarshal(payload, &s); err != nil {
			return nil
		}
		var out []reading
		add := func(metric string, v *float64) {
			if v != nil {
				out = append(out, reading{shellycaps.SwitchReadoutToken(ch, metric), *v})
			}
		}
		add("power", s.APower)
		add("voltage", s.Voltage)
		add("current", s.Current)
		add("freq", s.Freq)
		if s.Temp != nil {
			add("temp", s.Temp.C)
		}
		return out

	case gen1 && strings.HasPrefix(rest, "relay/") && strings.HasSuffix(rest, "/power"):
		// Gen1 has no combined status payload: power arrives alone, as a bare
		// number on its own topic. Voltage/current/frequency do not exist.
		ch, ok := atoiSuffix(strings.TrimSuffix(rest, "/power"), "relay/")
		if !ok {
			return nil
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(string(payload)), 64)
		if err != nil {
			return nil
		}
		return []reading{{shellycaps.SwitchReadoutToken(ch, "power"), v}}

	case gen1 && strings.HasSuffix(rest, "/status") &&
		(strings.HasPrefix(rest, "color/") || strings.HasPrefix(rest, "white/")):
		// Gen1 light (RGBW2). The editor's bindings against this topic set are
		// self-flagged VERIFICATION-PENDING against a real device; recording
		// follows the same shape, so if the live tree deviates the capture
		// wins for both.
		seg := "color/"
		if strings.HasPrefix(rest, "white/") {
			seg = "white/"
		}
		ch, ok := atoiSuffix(strings.TrimSuffix(rest, "/status"), seg)
		if !ok {
			return nil
		}
		var l gen1Light
		if err := json.Unmarshal(payload, &l); err != nil || l.Power == nil {
			return nil
		}
		return []reading{{shellycaps.LightReadoutToken(ch, "power"), *l.Power}}
	}
	return nil
}

// atoiSuffix parses the channel index that follows a fixed prefix.
func atoiSuffix(s, prefix string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(s, prefix))
	if err != nil || n < 0 || n > 99 {
		return 0, false
	}
	return n, true
}
