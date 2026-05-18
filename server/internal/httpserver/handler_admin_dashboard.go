package httpserver

import (
	"database/sql"
	"net/http"
	"strings"
	"time"

	"carvilon.local/server/internal/doorhistory"
	"carvilon.local/server/internal/mockmanager"
)

// adminUser is the {Name, Initials} bag.
type adminUser struct {
	Name     string
	Initials string
}

// adminDashboardData traegt die KPI-Karten plus zwei Listen
// (Klingel-Events + Login-Audit). Saison 13-02-FIX4-a-HOTFIX3:
// alle Zahlen kommen aus der DB, keine Fake-Werte mehr.
// Saison 13-02-FIX4-a-HOTFIX5: ESP-Viewer-Kachel (Live-Stats)
// plus ESP-Pager-Platzhalter.
// Saison 14-04-Phase2: AllViewers + SelectedMACs + AnyFilter
// versorgen das Filter-Dropdown im Dashboard-Header. AllViewers
// ist die Liste fuer das Multi-Select, SelectedMACs ist der aktuell
// aktive Filter, AnyFilter true wenn mindestens ein Viewer
// abgewaehlt wurde.
type adminDashboardData struct {
	User              adminUser
	WebViewersTotal   int
	WebViewersRunning int
	ActiveSessions    int
	EventsToday       int
	Events7d          int

	ESPAdopted       int
	ESPWithToken     int
	ESPPending       int
	ESPRejected      int
	ESPLastDiscovery string

	RecentEvents      []dashRecentEvent
	RecentEventsEmpty bool
	RecentAudit       []dashAuditRow
	RecentAuditEmpty  bool

	AllViewers    []dashViewerOption
	SelectedMACs  map[string]bool
	AnyFilter     bool
}

// dashViewerOption ist ein Eintrag im Filter-Dropdown.
type dashViewerOption struct {
	MAC      string
	Name     string
	Selected bool
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
		User:         adminUser{Name: username, Initials: initialsOf(username)},
		SelectedMACs: map[string]bool{},
	}

	// Saison 14-04-Phase2: ?viewer_macs=mac1,mac2 filtert die
	// "Letzte 20 Klingel-Anrufe"-Liste. Unbekannte / falsch-
	// formatierte MACs werden still verworfen (kein 400 - das
	// Filter-UI ist eine Bequemlichkeit, kein Auth-Surface).
	selectedSet := parseViewerMACsFilter(r.URL.Query().Get("viewer_macs"))
	data.SelectedMACs = selectedSet
	data.AnyFilter = len(selectedSet) > 0

	now := time.Now()
	infos, _ := s.mockMgr.ListViewers(r.Context())
	infoByMAC := map[string]mockmanager.ViewerInfo{}
	for _, info := range infos {
		infoByMAC[info.MAC] = info
		switch info.Type {
		case mockmanager.TypeWeb:
			data.WebViewersTotal++
			if info.Running {
				data.WebViewersRunning++
			}
		case mockmanager.TypeESP:
			data.ESPAdopted++
			if info.HasESPToken {
				data.ESPWithToken++
			}
		}
		data.AllViewers = append(data.AllViewers, dashViewerOption{
			MAC:      info.MAC,
			Name:     info.Name,
			Selected: selectedSet[info.MAC] || !data.AnyFilter,
		})
	}

	// ESP-Pending-Statistik plus juengster Discovery-Zeitstempel
	// kommen direkt aus esp_pending_devices. Stiller Fail (Tabelle
	// fehlt) blendet die Kachel-Werte einfach mit 0 aus.
	if s.platformCfg != nil {
		var (
			pending     sql.NullInt64
			rejected    sql.NullInt64
			latestDisco sql.NullInt64
		)
		_ = s.platformCfg.DB().QueryRowContext(r.Context(),
			`SELECT
			   COALESCE(SUM(CASE WHEN rejected_at IS NULL THEN 1 ELSE 0 END), 0),
			   COALESCE(SUM(CASE WHEN rejected_at IS NOT NULL THEN 1 ELSE 0 END), 0),
			   MAX(discovered_at)
			 FROM esp_pending_devices`,
		).Scan(&pending, &rejected, &latestDisco)
		data.ESPPending = int(pending.Int64)
		data.ESPRejected = int(rejected.Int64)
		if latestDisco.Valid && latestDisco.Int64 > 0 {
			data.ESPLastDiscovery = formatRelativeGerman(time.UnixMilli(latestDisco.Int64), now)
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
		// Filter weiterreichen wenn der Admin ein Subset ausgewaehlt
		// hat. AnyFilter=false (= "alle Viewer") laesst den
		// variadic-Slice leer; ListRecent verhaelt sich dann wie
		// pre-Saison-14-04-Phase2.
		var filterArg []string
		if data.AnyFilter {
			filterArg = make([]string, 0, len(selectedSet))
			for mac := range selectedSet {
				filterArg = append(filterArg, mac)
			}
		}
		recent, _ := s.history.ListRecent(r.Context(), recentEventsLimit, filterArg...)
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

// parseViewerMACsFilter validiert und kanonisiert das
// ?viewer_macs=mac1,mac2 Query-Parameter. Liefert ein Set zum
// schnellen Lookup. Unbekannte / fehlerhafte MACs werden still
// gedroppt - das Filter-UI ist eine Bequemlichkeit, kein
// Sicherheits-Gate; der Admin sieht so oder so alles.
func parseViewerMACsFilter(raw string) map[string]bool {
	out := map[string]bool{}
	if strings.TrimSpace(raw) == "" {
		return out
	}
	for _, p := range strings.Split(raw, ",") {
		mac := strings.ToLower(strings.TrimSpace(p))
		if !macFormat.MatchString(mac) {
			continue
		}
		out[mac] = true
	}
	return out
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
