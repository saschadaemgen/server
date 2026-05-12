package httpserver

import (
	"context"
	"net/http"
	"time"

	"unifix.local/server/internal/platformconfig"
)

type adminDashboardData struct {
	Title          string
	ShowNav        bool
	AdminName      string
	MockCount      int
	UserCount      int
	UserCountReady bool
	SchemaVersion  int
	ServerIPv4     string
	DevMode        bool
	Doorbell       adminDoorbellStats
}

// adminDoorbellStats is the dashboard slice of doorhistory data.
// Saison 13-01 puts the three rolling-window totals plus a
// per-mock 24h ranking on the dashboard card.
type adminDoorbellStats struct {
	Ready    bool
	Total24h int
	Total7d  int
	Total30d int
	TopMocks []adminDoorbellMockRow
}

type adminDoorbellMockRow struct {
	MAC   string
	Name  string
	Count int
}

func (s *Server) handleAdminDashboard(w http.ResponseWriter, r *http.Request) {
	username := AdminUserFromContext(r.Context())

	data := adminDashboardData{
		Title:      "Dashboard",
		ShowNav:    true,
		AdminName:  username,
		ServerIPv4: s.cfg.ServerIPv4,
		DevMode:    s.cfg.DevMode,
	}

	if infos, err := s.mockMgr.ListViewers(r.Context()); err == nil {
		data.MockCount = len(infos)
	}
	if v, err := s.currentSchemaVersion(r.Context()); err == nil {
		data.SchemaVersion = v
	}
	if s.ua != nil {
		if users, err := s.ua.ListUsers(r.Context()); err == nil {
			data.UserCount = len(users)
			data.UserCountReady = true
		}
	} else {
		// also count as ready if base URL + token are configured
		// but the lazy build did not happen yet.
		base, _ := s.platformCfg.Get(r.Context(), platformconfig.KeyUAAPIBaseURL)
		if base != "" {
			data.UserCountReady = true
		}
	}

	data.Doorbell = s.loadDoorbellStats(r.Context())

	s.renderAdminPage(w, "dashboard", data)
}

// loadDoorbellStats produces the dashboard card payload. Falls
// back to an empty (but Ready) struct on a store error so the
// card still renders; the operator sees zero counters rather
// than an admin 500.
func (s *Server) loadDoorbellStats(ctx context.Context) adminDoorbellStats {
	if s.history == nil {
		return adminDoorbellStats{Ready: false}
	}
	agg, err := s.history.AggregateAdmin(ctx, time.Now())
	if err != nil {
		s.log.Warn("doorhistory aggregate failed", "err", err)
		return adminDoorbellStats{Ready: false}
	}
	stats := adminDoorbellStats{
		Ready:    true,
		Total24h: agg.Total24h,
		Total7d:  agg.Total7d,
		Total30d: agg.Total30d,
	}
	if len(agg.PerMock24h) > 0 {
		stats.TopMocks = s.topMocks24h(ctx, agg.PerMock24h, 5)
	}
	return stats
}

// topMocks24h joins the per-mock counters against mock_viewers
// for display names, sorts by descending count and truncates to
// limit. Missing mocks (FK CASCADE just removed them mid-render)
// fall back to "(geloescht)".
func (s *Server) topMocks24h(ctx context.Context, counts map[string]int, limit int) []adminDoorbellMockRow {
	rows := make([]adminDoorbellMockRow, 0, len(counts))
	for mac, n := range counts {
		name := "(geloescht)"
		if info, err := s.mockMgr.GetViewerInfo(ctx, mac); err == nil {
			name = info.Name
		}
		rows = append(rows, adminDoorbellMockRow{MAC: mac, Name: name, Count: n})
	}
	// stable sort: descending by count, ties broken by mac for
	// deterministic dashboard output across reloads.
	sortDoorbellRows(rows)
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
}

func sortDoorbellRows(rows []adminDoorbellMockRow) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0; j-- {
			if rows[j].Count > rows[j-1].Count ||
				(rows[j].Count == rows[j-1].Count && rows[j].MAC < rows[j-1].MAC) {
				rows[j-1], rows[j] = rows[j], rows[j-1]
			} else {
				break
			}
		}
	}
}

// currentSchemaVersion is a thin DB helper kept on Server so we
// do not need to expose it on the db package.
func (s *Server) currentSchemaVersion(ctx context.Context) (int, error) {
	if s.platformCfg == nil {
		return 0, nil
	}
	// any service with a *db.DB works; reuse platformconfig's.
	// But we need the raw DB; ask via mockMgr is also fine.
	// Cleanest: just SELECT directly via the sessions service which
	// also has the DB. Saison 12-04 keeps this small.
	row := s.sessionsDB().QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`)
	var v int
	if err := row.Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}
