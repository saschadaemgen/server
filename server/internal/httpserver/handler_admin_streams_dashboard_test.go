package httpserver

import (
	"bytes"
	"context"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"carvilon.local/server/internal/streams"
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
