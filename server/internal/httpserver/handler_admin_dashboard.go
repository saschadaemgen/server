package httpserver

import (
	"net/http"
	"strings"
	"time"

	"unifix.local/server/internal/doorhistory"
	"unifix.local/server/internal/mockmanager"
)

// adminUser is the {Name, Initials} bag.
type adminUser struct {
	Name     string
	Initials string
}

// adminDashboardData traegt die echten Werte fuer das Dashboard:
// Web-Viewer-Anzahl, Klingel-Events, aktive Sessions, Audit-Liste.
type adminDashboardData struct {
	User           adminUser
	WebViewers     int
	WebViewersOn   int
	EventsToday    int
	EventsTrend    string
	ActiveSessions int
	RecentEvents   []dashRecentEvent
	RecentAudit    []dashAuditRow
}

type dashRecentEvent struct {
	When      string
	UnitName  string
	UnitMark  string
	DoorName  string
	Action    string
	Status    string
}

type dashAuditRow struct {
	When     string
	User     string
	IP       string
	Outcome  string
	Realm    string
}

func (s *Server) handleAdminDashboard(w http.ResponseWriter, r *http.Request) {
	username := AdminUserFromContext(r.Context())

	data := adminDashboardData{
		User: adminUser{Name: username, Initials: initialsOf(username)},
	}

	infos, err := s.mockMgr.ListViewers(r.Context())
	if err == nil {
		for _, info := range infos {
			if info.Type != mockmanager.TypeWeb {
				continue
			}
			data.WebViewers++
			if info.Running {
				data.WebViewersOn++
			}
		}
	}

	if s.history != nil {
		if agg, err := s.history.AggregateAdmin(r.Context(), time.Now()); err == nil {
			data.EventsToday = agg.Total24h
			data.EventsTrend = trendArrow(agg.Total24h, agg.Total7d/7)
		}
		// Recent events: pull from the most-active mocks. Until a
		// global feed exists, take the first viewer's history.
		if len(infos) > 0 {
			recent, _ := s.history.ListForMock(r.Context(), infos[0].MAC, 10)
			for _, ev := range recent {
				data.RecentEvents = append(data.RecentEvents, dashRecentEvent{
					When:     ev.OccurredAt.Format("15:04"),
					UnitName: infos[0].Name,
					UnitMark: initialsOf(infos[0].Name),
					DoorName: doorNameFromIntercom(ev.IntercomMAC),
					Action:   "Klingel",
					Status:   eventStatusFor(ev),
				})
			}
		}
	}

	if s.sessions != nil {
		if n, err := s.sessions.ActiveCount(r.Context()); err == nil {
			data.ActiveSessions = n
		}
	}

	if s.audit != nil {
		entries, err := s.audit.Recent(r.Context(), "", 20)
		if err == nil {
			for _, e := range entries {
				data.RecentAudit = append(data.RecentAudit, dashAuditRow{
					When:    e.Timestamp.Local().Format("15:04:05"),
					User:    e.Username,
					IP:      e.IP,
					Outcome: string(e.Outcome),
					Realm:   string(e.Realm),
				})
			}
		}
	}

	s.renderAdminPage(w, "dashboard", data)
}

// trendArrow renders a tiny percent-trend string like "+12%" or "-8%".
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

// initialsOf takes "Sascha Daemgen" -> "SD".
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

