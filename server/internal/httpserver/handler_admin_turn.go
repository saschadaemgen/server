// Admin TURN/STUN/ICE menu (Saison 18-10). Read-only view of the cloud
// TURN relay: a config + live-stats + history panel. The data is
// edge-local: the cloud forwards telemetry over the side-channel, the
// edge persists it (turnstore) and caches the latest live snapshot.
//
// Topology note: the relay runs on the VPS but this page is served by
// the edge (the admin UI + DB live here). The live numbers are the last
// snapshot the cloud pushed; the page shows their age ("Stand vor Xs")
// and flips to "stale" when the cloud link goes quiet, so an old number
// is never presented as current.
//
// In the public build (no stream) no snapshot and no events ever
// arrive, so the page renders an honest "TURN nicht aktiv".
package httpserver

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"time"

	"carvilon.local/server/internal/turnstore"
)

// turnDashboard is the payload shared by the server-rendered initial
// page and the JSON poll. JSON tags are the contract the dashboard JS
// reads.
type turnDashboard struct {
	// SnapshotPresent is false until the first cloud snapshot arrives.
	SnapshotPresent bool `json:"snapshot_present"`
	// Stale is true when the last snapshot is older than the freshness
	// threshold (cloud link quiet). AgeSeconds is its age, edge-clocked.
	Stale      bool `json:"stale"`
	AgeSeconds int  `json:"age_seconds"`
	// Active means: a fresh snapshot AND the relay reports enabled. The
	// UI shows the live numbers only when Active.
	Active bool `json:"active"`

	Stats     turnstore.Snapshot   `json:"stats"`
	Events    []turnstore.Event    `json:"events"`
	ICEEvents []turnstore.ICEEvent `json:"ice_events"`
	// Note is the standing privacy/retention hint shown on the page.
	Note string `json:"note"`
}

// turnPageData backs templates/admin/turn.html. The dashboard is built
// client-side from DataJSON (embedded in <script id="td-data">) for an
// instant initial paint; the JS then live-polls /a/turn/stats.json.
type turnPageData struct {
	User     adminUser
	DataJSON template.JS
}

// buildTurnDashboard assembles the read-only TURN view. Degrades
// cleanly: no snapshot holder / no store (public build, or cloud never
// connected) yields an empty, non-active dashboard rather than an error.
func (s *Server) buildTurnDashboard(ctx context.Context) turnDashboard {
	d := turnDashboard{Note: "IPs gekürzt, 30 Tage Aufbewahrung."}

	if s.turnSnapshots != nil {
		if snap, recv, present := s.turnSnapshots.Get(); present {
			age, stale := turnstore.Freshness(recv, time.Now(), turnstore.DefaultStaleAfter)
			d.SnapshotPresent = true
			d.AgeSeconds = age
			d.Stale = stale
			d.Stats = snap
			d.Active = !stale && snap.Enabled
		}
	}

	if s.turnStore != nil {
		if ev, err := s.turnStore.RecentEvents(ctx, 50); err != nil {
			s.log.Warn("admin turn recent events", "err", err)
		} else {
			d.Events = ev
		}
		if ice, err := s.turnStore.RecentICEEvents(ctx, 50); err != nil {
			s.log.Warn("admin turn recent ice events", "err", err)
		} else {
			d.ICEEvents = ice
		}
	}
	return d
}

// handleAdminTurn renders the TURN admin page with the initial snapshot
// embedded for an instant paint. Route: GET /a/turn.
func (s *Server) handleAdminTurn(w http.ResponseWriter, r *http.Request) {
	username := AdminUserFromContext(r.Context())
	d := s.buildTurnDashboard(r.Context())
	raw, err := json.Marshal(d)
	if err != nil {
		s.log.Warn("admin turn marshal", "err", err)
		raw = []byte(`{"snapshot_present":false}`)
	}
	s.renderAdminPage(w, "turn", turnPageData{
		User:     adminUser{Name: username, Initials: initialsOf(username)},
		DataJSON: template.JS(raw),
	})
}

// handleAdminTurnStatsJSON serves the live dashboard payload for the JS
// poll. Route: GET /a/turn/stats.json (requireAdminSession).
func (s *Server) handleAdminTurnStatsJSON(w http.ResponseWriter, r *http.Request) {
	d := s.buildTurnDashboard(r.Context())
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(d)
}
