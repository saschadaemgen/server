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
	"html/template"
	"net"
	"net/http"
	"strings"
	"time"

	"carvilon.local/server/internal/streams"
	"carvilon.local/server/internal/streamstore"
)

// streamsPageData is the render payload for templates/admin/streams.html.
// The dashboard is built client-side from DataJSON (the full dashboard
// snapshot embedded in a <script id="sd-data">), so the page paints
// instantly without a round-trip; the JS then live-polls
// /a/streams/stats.json. User powers the admin nav.
type streamsPageData struct {
	User       adminUser
	Configured bool
	BackendURL string
	Error      string
	DataJSON   template.JS
}

// streamDashboard is the full payload for the streams dashboard, shared
// by the HTML render and the JSON poll. JSON tags are the contract the
// dashboard JS reads.
type streamDashboard struct {
	Configured  bool                `json:"configured"`
	BackendURL  string              `json:"backend_url,omitempty"`
	GeneratedAt string              `json:"generated_at,omitempty"`
	Error       string              `json:"error,omitempty"`
	Global      streams.GlobalStats `json:"global"`
	Cloud       cloudConsumers      `json:"cloud"`
	Profiles    []dashProfile       `json:"profiles"`
	Clients     []dashClient        `json:"clients"`
}

// cloudConsumers is the dashboard-level cloud-viewer status (S20 step 2):
// the live WHEP-subscriber counts the VPS pushes over the side-channel,
// shown SEPARATELY from the LAN consumers. Present is false until the first
// snapshot arrives; Stale flips when the side-channel goes quiet so an old
// number is never presented as current (mirrors the /a/turn panel). Total is
// the count across all streams; the per-profile split lands on
// dashProfile.CloudClients. Unassigned is the part of Total that landed on
// NO profile row (MAC without a resolvable viewer, or a cloud profile that
// is no longer in the profile list) - surfaced so the card and the column
// sum can never diverge silently (S20 step-2 KORREKTUR).
type cloudConsumers struct {
	Present    bool `json:"present"`
	Stale      bool `json:"stale"`
	AgeSeconds int  `json:"age_seconds"`
	Total      int  `json:"total"`
	Unassigned int  `json:"unassigned"`
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
	CloudClients   int     `json:"cloud_clients"` // cloud (WHEP) consumers, kept SEPARATE from Clients (LAN). S20 step 2.
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

	// Cloud-viewer counts (S20 step 2): the VPS counts WHEP subscribers per
	// stream and pushes them over the side-channel; the edge caches the latest
	// snapshot in s.streamSnapshots. Shown SEPARATELY from the LAN clients,
	// with a receive-time freshness so a quiet link reads "veraltet" rather
	// than presenting an old number (or a misleading fresh 0).
	if s.streamSnapshots != nil {
		if snap, recv, present := s.streamSnapshots.Get(); present {
			cloud, perProfile := cloudConsumerView(snap, recv, time.Now(), s.resolveCloudProfile(ctx))
			d.Cloud = cloud
			assigned := 0
			for i := range d.Profiles {
				d.Profiles[i].CloudClients = perProfile[d.Profiles[i].Name]
				assigned += d.Profiles[i].CloudClients
			}
			// Whatever did not land on a row (unresolvable MAC, or a resolved
			// profile name with no row) stays in the honest Total but is
			// surfaced as Unassigned - the card and the column sum must never
			// diverge silently (S20 step-2 KORREKTUR).
			d.Cloud.Unassigned = d.Cloud.Total - assigned
		}
	}

	for _, c := range stats.Clients {
		d.Clients = append(d.Clients, dashClient{ClientStats: c, Kind: deviceKind(c.RemoteAddr)})
	}
	return d
}

// cloudConsumerView folds a cloud-viewer snapshot into the dashboard-level
// status plus a per-cloud-profile consumer count. Pure (no clock, no DB): the
// caller passes now and a MAC->cloud-profile resolver, so it is unit-testable
// without a viewer store. A STALE snapshot yields a zero Total and an empty
// map - the dashboard then shows "veraltet" rather than presenting an old
// number (or a misleading fresh 0), per the step-2 briefing. The honest Total
// counts every stream's consumers even when a MAC no longer resolves to a
// profile (a viewer deleted mid-call); such counts simply do not land on a row.
func cloudConsumerView(snap streamstore.Snapshot, receivedAt, now time.Time, resolve func(mac string) (string, bool)) (cloudConsumers, map[string]int) {
	age, stale := streamstore.Freshness(receivedAt, now, streamstore.DefaultStaleAfter)
	cs := cloudConsumers{Present: true, Stale: stale, AgeSeconds: age}
	perProfile := map[string]int{}
	if stale {
		return cs, perProfile
	}
	for _, st := range snap.Streams {
		cs.Total += st.Consumers
		if name, ok := resolve(st.StreamID); ok {
			perProfile[name] += st.Consumers
		}
	}
	return cs, perProfile
}

// resolveCloudProfile returns a MAC -> cloud-profile-name resolver backed by
// the viewer manager - the SAME ResolveCloudStreamProfile the cloud WHIP
// TrackSource uses, so a stream's consumers attach to the profile that stream
// actually publishes. ok=false when the viewer manager is absent (the unit
// harness) or the MAC is unknown, so the count still lands in the cloud Total
// but not on a profile row.
func (s *Server) resolveCloudProfile(ctx context.Context) func(string) (string, bool) {
	return func(mac string) (string, bool) {
		if s.viewerMgr == nil {
			return "", false
		}
		info, err := s.viewerMgr.GetViewerInfo(ctx, mac)
		if err != nil {
			return "", false
		}
		return info.ResolveCloudStreamProfile(), true
	}
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
