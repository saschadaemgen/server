package stats

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRegister_AssignsMonotonicIDs(t *testing.T) {
	r := New()
	c1 := r.Register("a", "mjpeg", "1.1.1.1:1")
	c2 := r.Register("b", "h264_cbp", "2.2.2.2:2")
	if c1.ID >= c2.ID {
		t.Errorf("IDs not monotonic: c1=%d c2=%d", c1.ID, c2.ID)
	}
	if c1.Profile != "a" || c2.Codec != "h264_cbp" {
		t.Errorf("immutable fields wrong: %+v %+v", c1, c2)
	}
}

func TestRegister_ConnectedAtIsSet(t *testing.T) {
	r := New()
	before := time.Now()
	c := r.Register("x", "mjpeg", "")
	after := time.Now()
	if c.ConnectedAt.Before(before) || c.ConnectedAt.After(after) {
		t.Errorf("ConnectedAt %v outside [%v, %v]", c.ConnectedAt, before, after)
	}
}

func TestUnregister_RemovesFromCount(t *testing.T) {
	r := New()
	c := r.Register("a", "mjpeg", "")
	if r.Count() != 1 {
		t.Fatalf("Count = %d, want 1", r.Count())
	}
	r.Unregister(c)
	if r.Count() != 0 {
		t.Errorf("Count after Unregister = %d, want 0", r.Count())
	}
}

func TestUnregister_NilSafe(t *testing.T) {
	r := New()
	r.Unregister(nil) // must not panic
}

func TestUnregister_IdempotentForDoubleCall(t *testing.T) {
	r := New()
	c := r.Register("a", "mjpeg", "")
	r.Unregister(c)
	r.Unregister(c) // must not panic; Count stays 0
	if r.Count() != 0 {
		t.Errorf("Count = %d, want 0", r.Count())
	}
}

func TestRecordFrame_AccumulatesCounters(t *testing.T) {
	r := New()
	c := r.Register("a", "mjpeg", "")
	c.RecordFrame(1000)
	c.RecordFrame(500)
	c.RecordFrame(250)
	snap := r.Snapshot()
	if len(snap.Clients) != 1 {
		t.Fatalf("Clients len = %d, want 1", len(snap.Clients))
	}
	if got := snap.Clients[0].FramesSent; got != 3 {
		t.Errorf("FramesSent = %d, want 3", got)
	}
	if got := snap.Clients[0].BytesSent; got != 1750 {
		t.Errorf("BytesSent = %d, want 1750", got)
	}
}

func TestRecordDrop_Counted(t *testing.T) {
	r := New()
	c := r.Register("a", "mjpeg", "")
	c.RecordDrop()
	c.RecordDrop()
	snap := r.Snapshot()
	if got := snap.Clients[0].FramesDropped; got != 2 {
		t.Errorf("FramesDropped = %d, want 2", got)
	}
}

func TestRecordFrame_NilClientSafe(t *testing.T) {
	var c *Client
	c.RecordFrame(100) // must not panic
	c.RecordDrop()
}

func TestSnapshot_GlobalAggregatesAllClients(t *testing.T) {
	r := New()
	a := r.Register("a", "mjpeg", "")
	b := r.Register("b", "h264_cbp", "")
	a.RecordFrame(1000)
	a.RecordFrame(1000)
	b.RecordFrame(500)
	snap := r.Snapshot()
	if snap.Global.Clients != 2 {
		t.Errorf("Global.Clients = %d, want 2", snap.Global.Clients)
	}
	if snap.Global.FramesSentTotal != 3 {
		t.Errorf("Global.FramesSentTotal = %d, want 3", snap.Global.FramesSentTotal)
	}
	if snap.Global.BytesSentTotal != 2500 {
		t.Errorf("Global.BytesSentTotal = %d, want 2500", snap.Global.BytesSentTotal)
	}
}

func TestSnapshot_PerProfileAggregation(t *testing.T) {
	// Two clients on profile "mjpeg_bal", one on "h264_cbp". Verify
	// the per-profile aggregation picks the right ones.
	r := New()
	a1 := r.Register("mjpeg_bal", "mjpeg", "1")
	a2 := r.Register("mjpeg_bal", "mjpeg", "2")
	b := r.Register("h264_cbp", "h264_cbp", "3")
	a1.RecordFrame(100)
	a2.RecordFrame(100)
	a2.RecordFrame(100)
	b.RecordFrame(500)
	snap := r.Snapshot()

	ps, ok := snap.Profiles["mjpeg_bal"]
	if !ok {
		t.Fatalf("Profiles missing mjpeg_bal: %+v", snap.Profiles)
	}
	if ps.Clients != 2 {
		t.Errorf("mjpeg_bal Clients = %d, want 2", ps.Clients)
	}
	if ps.FramesSent != 3 {
		t.Errorf("mjpeg_bal FramesSent = %d, want 3", ps.FramesSent)
	}
	if ps.BytesSent != 300 {
		t.Errorf("mjpeg_bal BytesSent = %d, want 300", ps.BytesSent)
	}
	if ps.Codec != "mjpeg" {
		t.Errorf("mjpeg_bal Codec = %q, want mjpeg", ps.Codec)
	}

	ps2 := snap.Profiles["h264_cbp"]
	if ps2.Clients != 1 || ps2.BytesSent != 500 {
		t.Errorf("h264_cbp aggregation wrong: %+v", ps2)
	}
}

func TestSnapshot_ClientsSortedByID(t *testing.T) {
	r := New()
	c1 := r.Register("a", "mjpeg", "")
	c2 := r.Register("b", "mjpeg", "")
	c3 := r.Register("c", "mjpeg", "")
	snap := r.Snapshot()
	if len(snap.Clients) != 3 {
		t.Fatalf("Clients len = %d, want 3", len(snap.Clients))
	}
	if snap.Clients[0].ID != c1.ID || snap.Clients[1].ID != c2.ID || snap.Clients[2].ID != c3.ID {
		t.Errorf("not sorted by ID: %+v", snap.Clients)
	}
}

func TestSnapshot_AvgFPSAndBitrate(t *testing.T) {
	// Construct a client with a synthetic ConnectedAt 2 seconds in the
	// past so the averages are deterministic.
	r := New()
	c := r.Register("a", "mjpeg", "")
	c.ConnectedAt = time.Now().Add(-2 * time.Second)
	c.RecordFrame(10000) // 10 kB
	c.RecordFrame(10000)
	c.RecordFrame(10000)
	c.RecordFrame(10000) // 4 frames, 40 kB
	snap := r.Snapshot()
	cs := snap.Clients[0]
	if cs.FramesSent != 4 {
		t.Fatalf("FramesSent = %d", cs.FramesSent)
	}
	// avg_fps ≈ 4/2 = 2.0
	if cs.AvgFPS < 1.8 || cs.AvgFPS > 2.2 {
		t.Errorf("AvgFPS = %v, want ~2.0", cs.AvgFPS)
	}
	// avg_bitrate_kbps = 40000*8/1000 / 2 = 160
	if cs.AvgBitrateKbps < 155 || cs.AvgBitrateKbps > 165 {
		t.Errorf("AvgBitrateKbps = %v, want ~160", cs.AvgBitrateKbps)
	}
}

func TestSnapshot_JSONRoundTrip(t *testing.T) {
	// Lock the JSON shape — the admin UI parses these tag names.
	r := New()
	c := r.Register("mjpeg_bal", "mjpeg", "10.0.0.5:54321")
	c.RecordFrame(800)
	buf, err := json.Marshal(r.Snapshot())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, want := range []string{
		`"generated_at":`,
		`"global":`,
		`"profiles":`,
		`"clients":`,
		`"frames_sent":1`,
		`"bytes_sent":800`,
		`"avg_fps":`,
		`"avg_bitrate_kbps":`,
		`"profile":"mjpeg_bal"`,
		`"codec":"mjpeg"`,
		`"remote_addr":"10.0.0.5:54321"`,
	} {
		if !strings.Contains(string(buf), want) {
			t.Errorf("JSON missing %q in:\n%s", want, buf)
		}
	}
}

func TestRecordFrame_ConcurrentSafe(t *testing.T) {
	// Race-detector smoke: 8 goroutines hammering RecordFrame on the
	// same client. Each goroutine does 1000 frames; total must be
	// exactly 8000 (no lost increments).
	r := New()
	c := r.Register("a", "mjpeg", "")
	const goroutines = 8
	const each = 1000
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				c.RecordFrame(1)
			}
		}()
	}
	wg.Wait()
	snap := r.Snapshot()
	if snap.Clients[0].FramesSent != goroutines*each {
		t.Errorf("FramesSent = %d, want %d", snap.Clients[0].FramesSent, goroutines*each)
	}
}

func TestRegister_ConcurrentSafe(t *testing.T) {
	// 50 goroutines each Register+Unregister. The registry's nextID
	// must end up at >=50 and the final Count must be 0.
	r := New()
	var wg sync.WaitGroup
	const n = 50
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			c := r.Register("a", "mjpeg", "")
			r.Unregister(c)
		}()
	}
	wg.Wait()
	if r.Count() != 0 {
		t.Errorf("Count = %d after Register+Unregister storm, want 0", r.Count())
	}
}
