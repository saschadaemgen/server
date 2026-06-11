package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"carvilon.local/server/internal/sidechannel"
	"carvilon.local/server/internal/streams"
	"carvilon.local/server/internal/streamstore"
	"carvilon.local/server/internal/viewermanager"
)

// TestBuildStreamsDashboard_PerProfileConsumers proves the consumer
// fix end-to-end: a profile's consumer count comes from
// /stream/stats.profiles[name].clients (per-profile), idle profiles
// (absent from stats) read zero and are inactive. Mirrors the real
// 31.05 numbers: mjpeg_bal 2, intercom_web 1, others 0.
func TestBuildStreamsDashboard_PerProfileConsumers(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/profiles":
			_, _ = w.Write([]byte(`[
				{"name":"intercom_web","codec":"h264_passthrough","fps":0},
				{"name":"mjpeg_bal","codec":"mjpeg","fps":12,"width":800,"height":1280},
				{"name":"intercom_android","codec":"h264_passthrough","fps":0}
			]`))
		case "/stream/stats":
			_, _ = w.Write([]byte(`{
				"generated_at":"2026-05-31T10:00:00Z",
				"global":{"clients":3,"frames_sent_total":10675,"bytes_sent_total":463182971},
				"profiles":{
					"intercom_web":{"profile":"intercom_web","codec":"h264_passthrough","clients":1,"avg_fps":30.04,"source_fps":30.03,"avg_bitrate_kbps":6004,"bytes_sent":135867555,"frames_sent":5438},
					"mjpeg_bal":{"profile":"mjpeg_bal","codec":"mjpeg","clients":2,"avg_fps":11.76,"source_fps":30.03,"avg_bitrate_kbps":5878,"bytes_sent":327315416,"frames_sent":5237}
				},
				"clients":[
					{"id":1,"profile":"intercom_web","remote_addr":"127.0.0.1:55658","uptime_sec":181,"avg_fps":30.04,"avg_bitrate_kbps":6004,"bytes_sent":135867555},
					{"id":2,"profile":"mjpeg_bal","remote_addr":"192.168.1.28:58949","uptime_sec":253,"avg_fps":11.58,"avg_bitrate_kbps":5793,"bytes_sent":183616873},
					{"id":3,"profile":"mjpeg_bal","remote_addr":"192.168.1.187:61669","uptime_sec":192,"avg_fps":11.99,"avg_bitrate_kbps":5989,"bytes_sent":143698543}
				]
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	c, _ := streams.New(backend.URL)
	s := &Server{streams: c, streamStats: c, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	d := s.buildStreamsDashboard(context.Background())
	if !d.Configured {
		t.Fatal("dashboard not configured")
	}
	want := map[string]struct {
		clients int
		active  bool
	}{
		"intercom_web":     {1, true},
		"mjpeg_bal":        {2, true},
		"intercom_android": {0, false},
	}
	got := map[string]dashProfile{}
	for _, p := range d.Profiles {
		got[p.Name] = p
	}
	for name, w := range want {
		p, ok := got[name]
		if !ok {
			t.Errorf("profile %q missing from dashboard", name)
			continue
		}
		if p.Clients != w.clients || p.Active != w.active {
			t.Errorf("%s: clients=%d active=%v, want clients=%d active=%v", name, p.Clients, p.Active, w.clients, w.active)
		}
	}
	if d.Global.Clients != 3 {
		t.Errorf("global clients = %d, want 3", d.Global.Clients)
	}
	if len(d.Clients) != 3 {
		t.Fatalf("want 3 clients, got %d", len(d.Clients))
	}
	// device-kind classification flows through to the payload.
	kinds := map[string]string{}
	for _, c := range d.Clients {
		kinds[c.RemoteAddr] = c.Kind
	}
	if kinds["127.0.0.1:55658"] != "loop" || kinds["192.168.1.28:58949"] != "esp" {
		t.Errorf("client kinds = %v", kinds)
	}
}

// TestRenderStreamsDashboardPage executes the real streams.html
// shell the way renderAdminPage does (navEnvelope + extractUser), so
// a template-execution error or a missing extractUser case (the nav
// user chip) cannot pass unit tests silently. Covers both the live
// dashboard and the not-configured card.
func TestRenderStreamsDashboardPage(t *testing.T) {
	tpl, err := newAdminTemplates()
	if err != nil {
		t.Fatalf("newAdminTemplates: %v", err)
	}

	render := func(page streamsPageData) string {
		t.Helper()
		env := navEnvelope{
			ActiveNav: navSlotFor("streams"),
			User:      extractUser(page),
			Page:      page,
		}
		var buf bytes.Buffer
		if err := tpl.renderPage(&buf, "streams", env); err != nil {
			t.Fatalf("render streams: %v", err)
		}
		return buf.String()
	}

	// Configured: the dashboard scaffold, the embedded snapshot, the
	// live-poll asset, and the nav user chip (proves extractUser knows
	// streamsPageData) must all be present.
	out := render(streamsPageData{
		User:       adminUser{Name: "operator", Initials: "OP"},
		Configured: true,
		BackendURL: "http://stream.local:8555",
		DataJSON:   template.JS(`{"configured":true,"profiles":[]}`),
	})
	for _, want := range []string{
		`id="sd-root"`,
		`data-poll="/a/streams/stats.json"`,
		`id="sd-data"`,
		`streams-dashboard.js`,
		`{"configured":true,"profiles":[]}`,
		"operator",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("configured page missing %q", want)
		}
	}

	// Not configured: the operator gets the setup card, never the live
	// scaffold.
	out = render(streamsPageData{
		User:       adminUser{Name: "operator", Initials: "OP"},
		Configured: false,
	})
	if !strings.Contains(out, "nicht konfiguriert") {
		t.Errorf("not-configured page missing setup card")
	}
	if strings.Contains(out, `id="sd-root"`) {
		t.Errorf("not-configured page must not render the live dashboard")
	}
}

func TestDeviceKind(t *testing.T) {
	cases := []struct {
		addr string
		want string
	}{
		{"127.0.0.1:55658", "loop"},
		{"[::1]:8080", "loop"},
		{"192.168.1.28:58949", "esp"},
		{"192.168.1.187:61669", "esp"}, // coarse rule: 192.168.x -> esp
		{"10.0.0.5:1234", "web"},
		{"172.16.4.9:443", "web"},
		{"203.0.113.7:5000", "web"},
		{"192.168.1.28", "esp"}, // no port still classifies
	}
	for _, c := range cases {
		if got := deviceKind(c.addr); got != c.want {
			t.Errorf("deviceKind(%q) = %q, want %q", c.addr, got, c.want)
		}
	}
}

// TestCloudConsumerView covers the pure fold (S20 step 2): a FRESH snapshot
// splits per cloud-profile and totals everything (incl. MACs that no longer
// resolve); a STALE snapshot presents no numbers ("veraltet", not a fresh 0);
// a fresh-but-empty snapshot is an honest zero (cloud up, 0 viewers).
func TestCloudConsumerView(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	resolve := func(mac string) (string, bool) {
		switch mac {
		case "aa", "bb":
			return "intercom_web", true
		case "cc":
			return "intercom_android", true
		}
		return "", false // unknown MAC (e.g. viewer deleted mid-call)
	}
	snap := streamstore.Snapshot{Streams: []streamstore.Stat{
		{StreamID: "aa", Consumers: 1},
		{StreamID: "bb", Consumers: 2},
		{StreamID: "cc", Consumers: 1},
		{StreamID: "zz", Consumers: 5}, // unresolved -> total only, no row
	}}

	cs, per := cloudConsumerView(snap, now.Add(-5*time.Second), now, resolve)
	if !cs.Present || cs.Stale {
		t.Fatalf("fresh: present/stale = %v/%v, want true/false", cs.Present, cs.Stale)
	}
	if cs.Total != 9 {
		t.Errorf("Total = %d, want 9 (1+2+1+5)", cs.Total)
	}
	if per["intercom_web"] != 3 || per["intercom_android"] != 1 {
		t.Errorf("per-profile = %v, want web=3 android=1", per)
	}
	if _, ok := per["zz"]; ok {
		t.Errorf("unresolved MAC must not appear as a profile key: %v", per)
	}

	cs2, per2 := cloudConsumerView(snap, now.Add(-60*time.Second), now, resolve)
	if !cs2.Present || !cs2.Stale {
		t.Errorf("stale: present/stale = %v/%v, want true/true", cs2.Present, cs2.Stale)
	}
	if cs2.Total != 0 || len(per2) != 0 {
		t.Errorf("stale must yield no numbers: Total=%d per=%v", cs2.Total, per2)
	}

	cs3, per3 := cloudConsumerView(streamstore.Snapshot{}, now, now, resolve)
	if !cs3.Present || cs3.Stale || cs3.Total != 0 || len(per3) != 0 {
		t.Errorf("fresh-empty: %+v per=%v", cs3, per3)
	}
}

// TestBuildStreamsDashboard_CloudConsumers proves the dashboard wiring: a fresh
// holder yields a present, non-stale cloud block with the honest Total; a stale
// holder reads present+stale with no numbers; no holder reads not-present. The
// viewer manager is nil here (per-profile mapping is covered by
// TestCloudConsumerView), proving buildStreamsDashboard is nil-safe on both
// the holder and the manager.
func TestBuildStreamsDashboard_CloudConsumers(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/profiles":
			_, _ = w.Write([]byte(`[{"name":"intercom_web","codec":"h264_passthrough","fps":0}]`))
		case "/stream/stats":
			_, _ = w.Write([]byte(`{"generated_at":"2026-06-09T10:00:00Z","global":{"clients":1},"profiles":{"intercom_web":{"profile":"intercom_web","clients":1}},"clients":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()
	c, _ := streams.New(backend.URL)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	holder := streamstore.NewSnapshotHolder()
	holder.Set(streamstore.Snapshot{Streams: []streamstore.Stat{
		{StreamID: "aa", Consumers: 2}, {StreamID: "bb", Consumers: 1},
	}}, time.Now())
	s := &Server{streams: c, streamStats: c, streamSnapshots: holder, log: log}
	d := s.buildStreamsDashboard(context.Background())
	if !d.Cloud.Present || d.Cloud.Stale {
		t.Fatalf("fresh cloud: %+v", d.Cloud)
	}
	if d.Cloud.Total != 3 {
		t.Errorf("cloud total = %d, want 3", d.Cloud.Total)
	}

	holder.Set(streamstore.Snapshot{Streams: []streamstore.Stat{{StreamID: "aa", Consumers: 2}}},
		time.Now().Add(-2*streamstore.DefaultStaleAfter))
	d = s.buildStreamsDashboard(context.Background())
	if !d.Cloud.Present || !d.Cloud.Stale {
		t.Errorf("stale cloud: present/stale = %v/%v, want true/true", d.Cloud.Present, d.Cloud.Stale)
	}
	if d.Cloud.Total != 0 {
		t.Errorf("stale cloud total = %d, want 0", d.Cloud.Total)
	}

	s2 := &Server{streams: c, streamStats: c, log: log}
	if d2 := s2.buildStreamsDashboard(context.Background()); d2.Cloud.Present {
		t.Errorf("no holder: cloud present = true, want false")
	}
}

// cloudRowTestBackend is the stream-backend stub for the profile-row tests:
// two configured profiles, no LAN clients. The cloud numbers come from the
// snapshot holder, never from /stream/stats, so the stats document stays empty.
func cloudRowTestBackend(t *testing.T) *streams.Client {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/profiles":
			_, _ = w.Write([]byte(`[
				{"name":"intercom_med","codec":"h264","fps":15},
				{"name":"intercom_web","codec":"h264_passthrough","fps":0}
			]`))
		case "/stream/stats":
			_, _ = w.Write([]byte(`{"generated_at":"2026-06-11T10:00:00Z","global":{"clients":0},"profiles":{},"clients":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(backend.Close)
	c, err := streams.New(backend.URL)
	if err != nil {
		t.Fatalf("streams.New: %v", err)
	}
	return c
}

// TestBuildStreamsDashboard_CloudConsumerOnProfileRow is the S20 step-2
// KORREKTUR autark test: a MAC-keyed cloud consumer stat must land on the
// PROFILE ROW of the dashboard table. Unlike TestCloudConsumerView (fake
// resolver) and TestBuildStreamsDashboard_CloudConsumers (nil manager), this
// pins the previously untested hop end to end: the REAL httpserver.New wiring
// (Deps.ViewerManager -> s.viewerMgr) and the REAL resolver chain the edge
// publish uses (viewermanager.GetViewerInfo -> ResolveCloudStreamProfile),
// against a real DB-backed viewer. Briefing scenario: sid=<mac>, count=1,
// viewer cloud_stream_profile=intercom_med -> row intercom_med carries
// Cloud=1, the card still Total=1. Two MACs on the SAME profile must sum.
func TestBuildStreamsDashboard_CloudConsumerOnProfileRow(t *testing.T) {
	env := newTestServer(t)
	ctx := context.Background()

	const mobiMAC = "0c:ea:14:20:00:01"
	if err := env.viewerMgr.AddViewer(ctx, viewermanager.ViewerSpec{
		MAC: mobiMAC, Name: "Mobi 4G", ServicePort: 8191, Type: viewermanager.TypeAndroid,
	}); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	if err := env.viewerMgr.SetCloudStreamProfile(ctx, mobiMAC, "intercom_med"); err != nil {
		t.Fatalf("SetCloudStreamProfile: %v", err)
	}

	c := cloudRowTestBackend(t)
	env.srv.streams, env.srv.streamStats = c, c
	holder := streamstore.NewSnapshotHolder()
	holder.Set(streamstore.Snapshot{Streams: []streamstore.Stat{
		{StreamID: mobiMAC, Consumers: 1},
	}}, time.Now())
	env.srv.streamSnapshots = holder

	rowCloud := func(d streamDashboard) map[string]int {
		out := map[string]int{}
		for _, p := range d.Profiles {
			out[p.Name] = p.CloudClients
		}
		return out
	}

	d := env.srv.buildStreamsDashboard(ctx)
	if !d.Cloud.Present || d.Cloud.Stale {
		t.Fatalf("cloud block = %+v, want present and fresh", d.Cloud)
	}
	if d.Cloud.Total != 1 {
		t.Errorf("card Total = %d, want 1 (card must stay correct)", d.Cloud.Total)
	}
	if d.Cloud.Unassigned != 0 {
		t.Errorf("Unassigned = %d, want 0 (the count landed on its row)", d.Cloud.Unassigned)
	}
	rows := rowCloud(d)
	if rows["intercom_med"] != 1 {
		t.Errorf("row intercom_med Cloud = %d, want 1 (rows=%v)", rows["intercom_med"], rows)
	}
	if rows["intercom_web"] != 0 {
		t.Errorf("row intercom_web Cloud = %d, want 0 (rows=%v)", rows["intercom_web"], rows)
	}

	// Second viewer with the SAME cloud profile: both MACs sum onto the row.
	const otherMAC = "0c:ea:14:20:00:02"
	if err := env.viewerMgr.AddViewer(ctx, viewermanager.ViewerSpec{
		MAC: otherMAC, Name: "Mobi Zwei", ServicePort: 8192, Type: viewermanager.TypeAndroid,
	}); err != nil {
		t.Fatalf("AddViewer 2: %v", err)
	}
	if err := env.viewerMgr.SetCloudStreamProfile(ctx, otherMAC, "intercom_med"); err != nil {
		t.Fatalf("SetCloudStreamProfile 2: %v", err)
	}
	holder.Set(streamstore.Snapshot{Streams: []streamstore.Stat{
		{StreamID: mobiMAC, Consumers: 1}, {StreamID: otherMAC, Consumers: 1},
	}}, time.Now())
	d = env.srv.buildStreamsDashboard(ctx)
	if rows = rowCloud(d); rows["intercom_med"] != 2 || d.Cloud.Total != 2 {
		t.Errorf("two MACs on one profile: row intercom_med = %d, Total = %d, want 2/2 (rows=%v)",
			rows["intercom_med"], d.Cloud.Total, rows)
	}
}

// TestBuildStreamsDashboard_CloudConsumerUnknownMAC: a stat for a MAC with no
// viewer must keep counting in the summary card (the card stays independently
// correct) but must not land on any profile row - and must not panic. Instead
// of silently diverging from the column sum it reads as Unassigned; the same
// holds when the viewer EXISTS but its cloud profile has no row anymore.
func TestBuildStreamsDashboard_CloudConsumerUnknownMAC(t *testing.T) {
	env := newTestServer(t)
	ctx := context.Background()

	c := cloudRowTestBackend(t)
	env.srv.streams, env.srv.streamStats = c, c
	holder := streamstore.NewSnapshotHolder()
	holder.Set(streamstore.Snapshot{Streams: []streamstore.Stat{
		{StreamID: "0c:ea:14:99:99:99", Consumers: 1}, // no such viewer
	}}, time.Now())
	env.srv.streamSnapshots = holder

	assertOffRows := func(d streamDashboard, when string) {
		t.Helper()
		if !d.Cloud.Present || d.Cloud.Stale {
			t.Fatalf("%s: cloud block = %+v, want present and fresh", when, d.Cloud)
		}
		if d.Cloud.Total != 1 {
			t.Errorf("%s: card Total = %d, want 1 (card counts independently)", when, d.Cloud.Total)
		}
		if d.Cloud.Unassigned != 1 {
			t.Errorf("%s: Unassigned = %d, want 1 (no silent card/column divergence)", when, d.Cloud.Unassigned)
		}
		for _, p := range d.Profiles {
			if p.CloudClients != 0 {
				t.Errorf("%s: row %s Cloud = %d, want 0 (must not fill a row)", when, p.Name, p.CloudClients)
			}
		}
	}
	assertOffRows(env.srv.buildStreamsDashboard(ctx), "unknown MAC")

	// Resolvable viewer whose cloud profile is gone from the profile list:
	// resolves to a name with no row -> counted, but Unassigned, no row.
	const ghostMAC = "0c:ea:14:99:99:98"
	if err := env.viewerMgr.AddViewer(ctx, viewermanager.ViewerSpec{
		MAC: ghostMAC, Name: "Mobi Geisterprofil", ServicePort: 8193, Type: viewermanager.TypeAndroid,
	}); err != nil {
		t.Fatalf("AddViewer: %v", err)
	}
	if err := env.viewerMgr.SetCloudStreamProfile(ctx, ghostMAC, "ghost_profile"); err != nil {
		t.Fatalf("SetCloudStreamProfile: %v", err)
	}
	holder.Set(streamstore.Snapshot{Streams: []streamstore.Stat{
		{StreamID: ghostMAC, Consumers: 1},
	}}, time.Now())
	assertOffRows(env.srv.buildStreamsDashboard(ctx), "missing profile row")
}

// TestBuildStreamsDashboard_CloudCountFromSidechannelPayload is the S20
// step-2 autark test: it carries a cloud consumer count of 1 from a SIMULATED
// side-channel payload all the way into the dashboard struct - 1 must arrive
// as 1, not 0. The payload is built exactly as the VPS builds it
// (sidechannel.Server.SendStreamStats marshals Envelope{Type, StreamStats}),
// decoded exactly as the edge client decodes it (wsjson = encoding/json into
// a fresh Envelope, the client.go TypeStreamStats dispatch guard), stored
// exactly as runEdge wires it (holder.Set with the edge receive time), then
// folded by buildStreamsDashboard. Pins the wire format on top: the frame
// must carry "stream_stats" with "consumers":1.
func TestBuildStreamsDashboard_CloudCountFromSidechannelPayload(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/profiles":
			_, _ = w.Write([]byte(`[{"name":"intercom_android","codec":"h264_passthrough","fps":0}]`))
		case "/stream/stats":
			_, _ = w.Write([]byte(`{"generated_at":"2026-06-11T10:00:00Z","global":{"clients":0},"profiles":{},"clients":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()
	c, _ := streams.New(backend.URL)

	// VPS side: the snapshot the cloud ticker builds from the whip counter
	// (buildStreamSnapshot shape) framed the way SendStreamStats sends it.
	wire, err := json.Marshal(sidechannel.Envelope{
		Type: sidechannel.TypeStreamStats,
		StreamStats: &streamstore.Snapshot{
			GeneratedAt: time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC),
			Streams:     []streamstore.Stat{{StreamID: "0c:ea:14:00:00:01", Consumers: 1}},
		},
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	for _, want := range []string{`"type":"stream_stats"`, `"consumers":1`, `"stream_id":"0c:ea:14:00:00:01"`} {
		if !strings.Contains(string(wire), want) {
			t.Fatalf("wire frame missing %s: %s", want, wire)
		}
	}

	// Edge side: decode + dispatch guard exactly as the client read loop does
	// (client.go case TypeStreamStats), store exactly as runEdge wires
	// OnStreamStats (holder.Set with the edge receive time).
	var env sidechannel.Envelope
	if err := json.Unmarshal(wire, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Type != sidechannel.TypeStreamStats || env.StreamStats == nil {
		t.Fatalf("frame would not dispatch: type=%q streamStats=%v", env.Type, env.StreamStats)
	}
	holder := streamstore.NewSnapshotHolder()
	holder.Set(*env.StreamStats, time.Now())

	// Dashboard: the value must surface as 1, not 0.
	s := &Server{streams: c, streamStats: c, streamSnapshots: holder,
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	d := s.buildStreamsDashboard(context.Background())
	if !d.Cloud.Present || d.Cloud.Stale {
		t.Fatalf("cloud block = %+v, want present and fresh", d.Cloud)
	}
	if d.Cloud.Total != 1 {
		t.Errorf("cloud total = %d, want 1 (the simulated VPS count must arrive intact)", d.Cloud.Total)
	}
}
