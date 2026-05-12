package httpserver

import (
	"context"
	"net/http"

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

	s.renderAdminPage(w, "dashboard", data)
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
