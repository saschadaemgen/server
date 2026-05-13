package httpserver

import (
	"context"
	"net/http"
	"strings"
	"time"

	"unifix.local/server/internal/doorhistory"
	"unifix.local/server/internal/platformconfig"
)

// adminUser is the {Name, Initials} bag the Claude-Design admin
// snippets expect in `.User`. We derive Initials from Name.
type adminUser struct {
	Name     string
	Initials string
}

// adminDashboardData carries the {{.User}}, {{.Stats}} and
// {{.RecentEvents}} fields the library admin-dashboard.html
// snippet renders.
type adminDashboardData struct {
	User         adminUser
	Stats        adminStats
	RecentEvents []adminRecentEvent
}

type adminStats struct {
	ActiveDevices int
	EventsToday   int
	EventsTrend   string // pre-formatted e.g. "+12%"
	CamerasOnline int
	CamerasTotal  int
	ActiveTenants int
}

type adminRecentEvent struct {
	When       string
	UnitName   string
	UnitMark   string
	TenantName string
	DoorName   string
	Action     string
	Status     string // "answered" | "ignored" | "missed" | "opened" | ...
}

func (s *Server) handleAdminDashboard(w http.ResponseWriter, r *http.Request) {
	username := AdminUserFromContext(r.Context())

	data := adminDashboardData{
		User: adminUser{Name: username, Initials: initialsOf(username)},
	}

	mocks, err := s.mockMgr.ListViewers(r.Context())
	if err == nil {
		data.Stats.ActiveDevices = len(mocks)
		data.Stats.CamerasTotal = len(mocks)
		for _, m := range mocks {
			if m.Running {
				data.Stats.CamerasOnline++
			}
		}
	}

	// UA-User count - fall back to ActiveDevices when UA-API is not configured.
	if s.ua != nil {
		if users, err := s.ua.ListUsers(r.Context()); err == nil {
			data.Stats.ActiveTenants = len(users)
		}
	} else {
		base, _ := s.platformCfg.Get(r.Context(), platformconfig.KeyUAAPIBaseURL)
		if base != "" {
			data.Stats.ActiveTenants = data.Stats.ActiveDevices
		}
	}

	// Door-event counters from doorhistory.
	if s.history != nil {
		if agg, err := s.history.AggregateAdmin(r.Context(), time.Now()); err == nil {
			data.Stats.EventsToday = agg.Total24h
			data.Stats.EventsTrend = trendArrow(agg.Total24h, agg.Total7d/7)
		}
		// Recent events: pull the most-recent N for whichever mock fired.
		// We pick the first mock's history as a proxy until a global feed exists.
		if len(mocks) > 0 {
			recent, _ := s.history.ListForMock(r.Context(), mocks[0].MAC, 10)
			for _, ev := range recent {
				data.RecentEvents = append(data.RecentEvents, adminRecentEvent{
					When:     ev.OccurredAt.Format("15:04"),
					UnitName: mocks[0].Name,
					UnitMark: initialsOf(mocks[0].Name),
					DoorName: doorNameFromIntercom(ev.IntercomMAC),
					Action:   "Klingel",
					Status:   eventStatusFor(ev),
				})
			}
		}
	}

	s.renderAdminPage(w, "dashboard", data)
}

// trendArrow renders a tiny percent-trend string like "+12%" or
// "-8%". Without baseline the string is a neutral "+0%".
func trendArrow(today int, baseline int) string {
	if baseline == 0 {
		if today == 0 {
			return "+0%"
		}
		return "+100%"
	}
	delta := (today - baseline) * 100 / baseline
	if delta >= 0 {
		return "+" + itoa(delta) + "%"
	}
	return itoa(delta) + "%"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func eventStatusFor(ev doorhistory.Event) string {
	switch {
	case ev.AnsweredAt != nil:
		return "answered"
	case ev.EndedAt != nil:
		return "answered"
	case ev.CancelledAt != nil:
		return "ignored"
	default:
		return "missed"
	}
}

func doorNameFromIntercom(mac string) string {
	if mac == "" {
		return "Hauseingang"
	}
	return mac
}

// initialsOf takes "Sascha Daemgen" -> "SD", "saschsa" -> "S",
// "" -> "?". Used for the avatar bubble in the admin nav and
// the per-row identity column.
func initialsOf(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "?"
	}
	parts := strings.Fields(name)
	if len(parts) == 1 {
		return strings.ToUpper(parts[0][:1])
	}
	first := strings.ToUpper(parts[0][:1])
	last := strings.ToUpper(parts[len(parts)-1][:1])
	return first + last
}

// currentSchemaVersion is kept for diagnostic uses; the Claude-
// Design dashboard does not surface it directly anymore.
func (s *Server) currentSchemaVersion(ctx context.Context) (int, error) {
	if s.platformCfg == nil {
		return 0, nil
	}
	row := s.sessionsDB().QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`)
	var v int
	if err := row.Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}
