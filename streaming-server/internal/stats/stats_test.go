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

// --- S6-04 source counter -----------------------------------------------------

func TestRecordSourceFrame_NilSafe(t *testing.T) {
	var r *Registry
	r.RecordSourceFrame("anything") // must not panic
	r.ResetSourceCounter("anything")
}

func TestRecordSourceFrame_EmptyProfileIsNoOp(t *testing.T) {
	r := New()
	r.RecordSourceFrame("") // must not panic, must not allocate counter
	// Counter table should still be empty; check via Snapshot — no
	// client => no profile entries => source data absent.
	snap := r.Snapshot()
	if len(snap.Profiles) != 0 {
		t.Errorf("Profiles len = %d, want 0", len(snap.Profiles))
	}
}

func TestSourceFPS_ShowsInSnapshotWhenProfileHasClient(t *testing.T) {
	// A profile only shows source data in the snapshot if it has at
	// least one client (matching the encoder-session-is-active
	// invariant the hubs honor).
	r := New()
	c := r.Register("mjpeg_bal", "mjpeg", "")
	// Pin ConnectedAt 1 s ago so avg_fps calc is stable.
	c.ConnectedAt = time.Now().Add(-time.Second)
	// Manually backdate the source-counter session-start to 1s ago
	// (in production the hub would have called RecordSourceFrame once
	// per upstream AU; we simulate 15 of them).
	for i := 0; i < 15; i++ {
		r.RecordSourceFrame("mjpeg_bal")
	}
	// Backdate sessionStartNs so source_fps comes out predictable.
	pc := r.profileCounter("mjpeg_bal")
	pc.sessionStartNs.Store(time.Now().Add(-time.Second).UnixNano())

	snap := r.Snapshot()
	ps, ok := snap.Profiles["mjpeg_bal"]
	if !ok {
		t.Fatalf("profile missing from snapshot")
	}
	if ps.SourceFrames != 15 {
		t.Errorf("SourceFrames = %d, want 15", ps.SourceFrames)
	}
	if ps.SourceFPS < 13 || ps.SourceFPS > 17 {
		t.Errorf("SourceFPS = %v, want ~15", ps.SourceFPS)
	}
}

func TestSourceFPS_DoesNotAppearWithoutClient(t *testing.T) {
	r := New()
	// Source frames recorded without any client. The Snapshot must
	// NOT surface this — there's no active session to attribute the
	// counter to from the consumer's perspective.
	r.RecordSourceFrame("ghost_profile")
	r.RecordSourceFrame("ghost_profile")
	snap := r.Snapshot()
	if _, ok := snap.Profiles["ghost_profile"]; ok {
		t.Errorf("orphaned source counter leaked into snapshot")
	}
}

func TestResetSourceCounter_ClearsBothFieldsForNextSession(t *testing.T) {
	r := New()
	c := r.Register("h264_cbp", "h264_cbp", "")
	c.ConnectedAt = time.Now().Add(-2 * time.Second)
	for i := 0; i < 30; i++ {
		r.RecordSourceFrame("h264_cbp")
	}
	if pc := r.profileCounter("h264_cbp"); pc.sourceFrames.Load() != 30 {
		t.Fatalf("pre-reset SourceFrames = %d, want 30", pc.sourceFrames.Load())
	}

	r.ResetSourceCounter("h264_cbp")
	pc := r.profileCounter("h264_cbp")
	if pc.sourceFrames.Load() != 0 {
		t.Errorf("post-reset SourceFrames = %d, want 0", pc.sourceFrames.Load())
	}
	if pc.sessionStartNs.Load() != 0 {
		t.Errorf("post-reset sessionStartNs = %d, want 0", pc.sessionStartNs.Load())
	}

	// First subsequent RecordSourceFrame re-arms the counter.
	r.RecordSourceFrame("h264_cbp")
	if pc.sourceFrames.Load() != 1 {
		t.Errorf("post-reset+record SourceFrames = %d, want 1", pc.sourceFrames.Load())
	}
	if pc.sessionStartNs.Load() == 0 {
		t.Errorf("post-reset+record sessionStartNs still zero")
	}
}

func TestRecordSourceFrame_ConcurrentSafe(t *testing.T) {
	r := New()
	const goroutines = 8
	const each = 1000
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				r.RecordSourceFrame("p")
			}
		}()
	}
	wg.Wait()
	pc := r.profileCounter("p")
	if got := pc.sourceFrames.Load(); got != goroutines*each {
		t.Errorf("SourceFrames = %d, want %d", got, goroutines*each)
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

// TestUplinkAccounting pins the S20 sender split: an uplink (the WHIP
// publish bridge) carries full throughput counters — it IS edge egress —
// but is counted under Uplinks, never under the Clients consumer counts,
// neither per profile nor globally. Unregistering it removes the profile
// entry entirely (the row reads idle again, no ghost).
func TestUplinkAccounting(t *testing.T) {
	r := New()
	viewer := r.Register("intercom_web", "h264_passthrough", "192.168.1.50:1234")
	up := r.RegisterUplink("intercom_med", "h264_reencode_shortgop", "whip-uplink")

	viewer.RecordFrame(1000)
	up.RecordFrame(2000)
	up.RecordFrame(2000)
	up.RecordDrop()

	snap := r.Snapshot()
	if snap.Global.Clients != 1 || snap.Global.Uplinks != 1 {
		t.Errorf("global clients/uplinks = %d/%d, want 1/1", snap.Global.Clients, snap.Global.Uplinks)
	}
	if snap.Global.FramesSentTotal != 3 || snap.Global.BytesSentTotal != 5000 {
		t.Errorf("global egress = %d frames / %d bytes, want 3/5000 (uplink included)",
			snap.Global.FramesSentTotal, snap.Global.BytesSentTotal)
	}

	med, ok := snap.Profiles["intercom_med"]
	if !ok {
		t.Fatal("intercom_med missing from profiles (uplink must create the entry)")
	}
	if med.Clients != 0 || med.Uplinks != 1 {
		t.Errorf("intercom_med clients/uplinks = %d/%d, want 0/1", med.Clients, med.Uplinks)
	}
	if med.FramesSent != 2 || med.FramesDropped != 1 || med.BytesSent != 4000 {
		t.Errorf("intercom_med egress = %d frames / %d drops / %d bytes, want 2/1/4000",
			med.FramesSent, med.FramesDropped, med.BytesSent)
	}
	if med.AvgFPS <= 0 || med.AvgBitrateKbps <= 0 {
		t.Errorf("intercom_med averages must be live: fps=%v kbps=%v", med.AvgFPS, med.AvgBitrateKbps)
	}
	web := snap.Profiles["intercom_web"]
	if web.Clients != 1 || web.Uplinks != 0 {
		t.Errorf("intercom_web clients/uplinks = %d/%d, want 1/0", web.Clients, web.Uplinks)
	}

	var uplinkSeen bool
	for _, cs := range snap.Clients {
		if cs.Uplink {
			uplinkSeen = true
			if cs.Profile != "intercom_med" || cs.RemoteAddr != "whip-uplink" {
				t.Errorf("uplink entry = %+v, want profile intercom_med / label whip-uplink", cs)
			}
		}
	}
	if !uplinkSeen {
		t.Error("uplink entry missing from the clients list")
	}

	// Wire contract for the carvilon dashboard: "uplinks" on the profile,
	// "uplink" on the entry — and both ABSENT for plain viewers (omitempty),
	// so the existing JSON shape stays byte-stable without uplinks.
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"uplinks":1`, `"uplink":true`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("snapshot JSON missing %s: %s", want, raw)
		}
	}
	if webRaw, _ := json.Marshal(snap.Profiles["intercom_web"]); strings.Contains(string(webRaw), "uplink") {
		t.Errorf("viewer-only profile must not carry uplink fields: %s", webRaw)
	}

	r.Unregister(up)
	snap = r.Snapshot()
	if snap.Global.Uplinks != 0 {
		t.Errorf("global uplinks after unregister = %d, want 0", snap.Global.Uplinks)
	}
	if _, ok := snap.Profiles["intercom_med"]; ok {
		t.Error("intercom_med still present after uplink unregister (ghost row)")
	}
}
