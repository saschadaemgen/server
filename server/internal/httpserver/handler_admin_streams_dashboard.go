// Admin streams dashboard data layer (Saison 17-14).
//
// buildStreamsDashboard merges the profile inventory (GET /api/profiles
// via the StreamBackend) with the live GET /stream/stats document
// (global aggregate + per-profile + per-client). It is the single
// source for both the server-rendered initial page (handler_admin_
// streams.go) and the JSON poll endpoint the dashboard JS hits every
// few seconds (GET /a/streams/stats.json). The browser only ever talks
// to carvilon - never the stream-server directly - so the auth/proxy
// boundary is preserved.
package httpserver

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"

	"carvilon.local/server/internal/streams"
)

// streamDashboard is the full payload for the streams dashboard, shared
// by the HTML render and the JSON poll. JSON tags are the contract the
// dashboard JS reads.
type streamDashboard struct {
	Configured  bool                `json:"configured"`
	BackendURL  string              `json:"backend_url,omitempty"`
	GeneratedAt string              `json:"generated_at,omitempty"`
	Error       string              `json:"error,omitempty"`
	Global      streams.GlobalStats `json:"global"`
	Profiles    []dashProfile       `json:"profiles"`
	Clients     []dashClient        `json:"clients"`
}

// dashProfile is one profile row: its persisted config (from
// /api/profiles) joined with its live numbers (from /stream/stats, all
// zero when the profile is idle). Active is true when it currently has
// at least one consumer (lazy pull: the source starts at the first
// subscriber).
type dashProfile struct {
	Name        string `json:"name"`
	Codec       string `json:"codec"`
	Usage       string `json:"usage"`
	Description string `json:"description"`
	CameraID    string `json:"camera_id"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	TargetFPS   int    `json:"target_fps"` // profile.FPS; 0 for passthrough
	Encryption  string `json:"encryption"`

	Active         bool    `json:"active"`
	Clients        int     `json:"clients"`
	FramesSent     int64   `json:"frames_sent"`
	FramesDropped  int64   `json:"frames_dropped"`
	BytesSent      int64   `json:"bytes_sent"`
	AvgFPS         float64 `json:"avg_fps"`
	SourceFPS      float64 `json:"source_fps"`
	AvgBitrateKbps float64 `json:"avg_bitrate_kbps"`
}

// dashClient is one connected consumer plus a server-computed device
// kind for the UI (so the heuristic lives in one place, not duplicated
// in JS). The embedded ClientStats flattens its JSON fields alongside
// "kind".
type dashClient struct {
	streams.ClientStats
	Kind string `json:"kind"` // esp | web | loop
}

// buildStreamsDashboard assembles the dashboard payload. Degrades
// gracefully: an unreachable stats endpoint yields zeroed live numbers
// (every profile idle) but still lists the configured profiles; an
// unreachable profile list sets Error and returns what it has.
func (s *Server) buildStreamsDashboard(ctx context.Context) streamDashboard {
	d := streamDashboard{}
	if !s.streams.Configured() {
		return d
	}
	d.Configured = true
	if u, ok := s.streams.(baseURLer); ok {
		d.BackendURL = u.BaseURL()
	}
	profiles, err := s.streams.List(ctx)
	if err != nil {
		s.log.Warn("admin streams dashboard list", "err", err)
		d.Error = err.Error()
		return d
	}
	stats := s.fetchStreamStats(ctx)
	d.GeneratedAt = stats.GeneratedAt
	d.Global = stats.Global

	for _, p := range profiles {
		st := stats.Profiles[p.Name] // zero value when idle/absent
		dp := dashProfile{
			Name:        p.Name,
			Codec:       p.Codec,
			Usage:       p.Usage,
			Description: p.Description,
			CameraID:    p.CameraID,
			Width:       p.Width,
			Height:      p.Height,
			TargetFPS:   p.FPS,
			Encryption:  p.Encryption,
			Active:      st.Clients > 0,
		}
		if dp.Active {
			dp.Clients = st.Clients
			dp.FramesSent = st.FramesSent
			dp.FramesDropped = st.FramesDropped
			dp.BytesSent = st.BytesSent
			dp.AvgFPS = st.AvgFPS
			dp.SourceFPS = st.SourceFPS
			dp.AvgBitrateKbps = st.AvgBitrateKbps
		}
		d.Profiles = append(d.Profiles, dp)
	}

	for _, c := range stats.Clients {
		d.Clients = append(d.Clients, dashClient{ClientStats: c, Kind: deviceKind(c.RemoteAddr)})
	}
	return d
}

// deviceKind classifies a client's remote address for the dashboard
// icon/tag. It is a DISPLAY hint, not a security signal:
//
//	loopback (127.0.0.1 / ::1) -> "loop"  (the carvilon WebRTC /offer
//	                                        proxy connecting to :8555)
//	192.168.x.x                -> "esp"   (the ESP indoor monitors)
//	anything else              -> "web"   (browser / other LAN client)
//
// Two LAN clients on the same subnet cannot always be told apart from
// the address alone; this is the coarse, briefing-specified rule.
func deviceKind(remoteAddr string) string {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return "loop"
	}
	if strings.HasPrefix(host, "192.168.") {
		return "esp"
	}
	return "web"
}

// handleAdminStreamsStatsJSON serves the live dashboard payload for the
// JS poll. Route: GET /a/streams/stats.json (requireAdminSession).
func (s *Server) handleAdminStreamsStatsJSON(w http.ResponseWriter, r *http.Request) {
	d := s.buildStreamsDashboard(r.Context())
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(d)
}
