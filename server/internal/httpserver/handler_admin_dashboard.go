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

// adminDashboardData traegt die vier KPI-Karten plus zwei Listen
// (Klingel-Events + Login-Audit). Saison 13-02-FIX4-a-HOTFIX3:
// alle Zahlen kommen aus der DB, keine Fake-Werte mehr.
type adminDashboardData struct {
	User                adminUser
	WebViewersTotal     int
	WebViewersRunning   int
	ActiveSessions      int
	EventsToday         int
	Events7d            int
	RecentEvents        []dashRecentEvent
	RecentEventsEmpty   bool
	RecentAudit         []dashAuditRow
	RecentAuditEmpty    bool
}

type dashRecentEvent struct {
	When      string
	ViewerMAC string
	UnitName  string
	UnitMark  string
	DoorName  string
	Status    string
}

type dashAuditRow struct {
	When    string
	User    string
	IP      string
	Outcome string
	Realm   string
}

const (
	recentEventsLimit = 20
	recentAuditLimit  = 10
	activeWindow      = 30 * time.Minute
)

func (s *Server) handleAdminDashboard(w http.ResponseWriter, r *http.Request) {
	username := AdminUserFromContext(r.Context())

	data := adminDashboardData{
		User: adminUser{Name: username, Initials: initialsOf(username)},
	}

	now := time.Now()
	infos, _ := s.mockMgr.ListViewers(r.Context())
	infoByMAC := map[string]mockmanager.ViewerInfo{}
	for _, info := range infos {
		infoByMAC[info.MAC] = info
		if info.Type != mockmanager.TypeWeb {
			continue
		}
		data.WebViewersTotal++
		if info.Running {
			data.WebViewersRunning++
		}
	}

	if s.sessions != nil {
		if n, err := s.sessions.RecentlyActiveCount(r.Context(), activeWindow); err == nil {
			data.ActiveSessions = n
		}
	}

	if s.history != nil {
		startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		if n, err := s.history.CountSince(r.Context(), startOfToday); err == nil {
			data.EventsToday = n
		}
		if n, err := s.history.CountSince(r.Context(), now.Add(-7*24*time.Hour)); err == nil {
			data.Events7d = n
		}
		recent, _ := s.history.ListRecent(r.Context(), recentEventsLimit)
		for _, ev := range recent {
			row := dashRecentEvent{
				When:      formatRelativeGerman(ev.OccurredAt, now),
				ViewerMAC: ev.MockMAC,
				UnitName:  ev.MockMAC,
				DoorName:  doorNameFromIntercom(ev.IntercomMAC),
				Status:    eventStatusFor(ev),
			}
			if info, ok := infoByMAC[ev.MockMAC]; ok && info.Name != "" {
				row.UnitName = info.Name
				row.UnitMark = initialsOf(info.Name)
			} else {
				row.UnitMark = "?"
			}
			data.RecentEvents = append(data.RecentEvents, row)
		}
		data.RecentEventsEmpty = len(data.RecentEvents) == 0
	} else {
		data.RecentEventsEmpty = true
	}

	if s.audit != nil {
		entries, _ := s.audit.Recent(r.Context(), "", recentAuditLimit)
		for _, e := range entries {
			data.RecentAudit = append(data.RecentAudit, dashAuditRow{
				When:    e.Timestamp.Local().Format("02.01. 15:04:05"),
				User:    e.Username,
				IP:      e.IP,
				Outcome: string(e.Outcome),
				Realm:   string(e.Realm),
			})
		}
	}
	data.RecentAuditEmpty = len(data.RecentAudit) == 0

	s.renderAdminPage(w, "dashboard", data)
}

// formatRelativeGerman rendert eine relative Zeit-Angabe fuer
// Dashboard-Listen: "vor 12 Sek", "vor 3 Min", "vor 2 h", "vor 4 d".
// Aelteres ($ge 7 Tage) wird absolut formatiert.
func formatRelativeGerman(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "vor " + itoa(int(d.Seconds())) + " Sek"
	case d < time.Hour:
		return "vor " + itoa(int(d.Minutes())) + " Min"
	case d < 24*time.Hour:
		return "vor " + itoa(int(d.Hours())) + " h"
	case d < 7*24*time.Hour:
		return "vor " + itoa(int(d.Hours()/24)) + " d"
	default:
		return t.Local().Format("02.01.06 15:04")
	}
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
		return "beantwortet"
	case ev.EndedAt != nil:
		return "beantwortet"
	case ev.CancelledAt != nil:
		return "abgebrochen"
	default:
		return "verpasst"
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

