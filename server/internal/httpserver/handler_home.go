package httpserver

import (
	"context"
	"errors"
	"net/http"
	"time"

	"unifix.local/server/internal/doorhistory"
	"unifix.local/server/internal/mockmanager"
	"unifix.local/server/internal/weather"
)

// ViewerHistoryLimit caps the number of door_events shown on the
// /m/ list.
const ViewerHistoryLimit = 20

// viewerHomeData is the payload for the Claude-Design intercom
// snippets. The library uses three pages of fields under one
// flat struct; we mirror their names exactly so the snippets
// can be reused unchanged.
//
// Saison 13-07 dropped StandbyDoorID; the standby-unlock JS
// now POSTs to the literal /webviewer/doors/standby/unlock
// route and the server reads viewer.paired_intercom_mac.
type viewerHomeData struct {
	UnitName     string
	DoorName     string
	Now          string // "HH:MM:SS"
	NowDate      string // "Di, 13. Mai"
	DND          bool
	HasUnread    bool
	HistoryItems []viewerHistoryRow // {Where, When, Unread}
	// Saison 14-01b idle-view fields.
	IdleViewMode string            // "screensaver" or "livestream"
	Weather      *weather.Snapshot // nil = backend unreachable, hide weather block
	// Saison 14-03 inline-mode payload. AutoScreensaverSeconds is
	// the persisted timer (0 = disabled); the browser runtime
	// promotes the setting into the slide-up modes container.
	AutoScreensaverSeconds int
}

// viewerHistoryRow matches the design-library shape for one
// history-sheet entry.
type viewerHistoryRow struct {
	Where  string
	When   string
	Unread bool
}

// handleHome renders the tenant intercom-viewer page (the
// Claude-Design library produces the markup; we provide data).
//
// Saison 13-01 Mark-Read-Variante-A bleibt aktiv: nach dem
// Rendern werden die angezeigten ungelesenen Events asynchron
// als gelesen markiert.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	mac := ViewerMACFromContext(r.Context())
	if mac == "" {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	info, err := s.mockMgr.GetViewerInfo(r.Context(), mac)
	if err != nil {
		if errors.Is(err, mockmanager.ErrViewerNotFound) {
			s.clearSessionCookie(w)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		s.log.Error("get viewer info", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	history, unread := s.loadViewerHistory(r.Context(), mac)
	rows := make([]viewerHistoryRow, 0, len(history))
	displayedIDs := make([]int64, 0, len(history))
	for _, ev := range history {
		where := ev.IntercomMAC
		if where == "" {
			where = "Hauseingang"
		}
		rows = append(rows, viewerHistoryRow{
			Where:  where,
			When:   formatGermanWhen(ev.OccurredAt),
			Unread: ev.ReadAt == nil,
		})
		if ev.ReadAt == nil {
			displayedIDs = append(displayedIDs, ev.ID)
		}
	}

	now := time.Now()
	data := viewerHomeData{
		UnitName:               info.Name,
		DoorName:               "Hauseingang",
		Now:                    now.Format("15:04:05"),
		NowDate:                formatGermanDate(now),
		DND:                    false,
		HasUnread:              unread > 0,
		HistoryItems:           rows,
		IdleViewMode:           info.ResolveIdleViewMode(),
		Weather:                s.fetchHomeWeather(r),
		AutoScreensaverSeconds: info.ResolveAutoScreensaverSeconds(),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.renderViewer(w, "home", data); err != nil {
		s.log.Error("render viewer home", "err", err)
		return
	}

	if len(displayedIDs) > 0 && s.history != nil {
		go func(ids []int64) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.history.MarkRead(ctx, mac, ids); err != nil {
				s.log.Warn("doorhistory mark read failed", "mac", mac, "err", err)
			}
		}(displayedIDs)
	}
}

// loadViewerHistory returns the list plus unread count, both 0 if
// the history store is not wired.
func (s *Server) loadViewerHistory(ctx context.Context, mac string) ([]doorhistory.Event, int) {
	if s.history == nil {
		return nil, 0
	}
	list, err := s.history.ListForMock(ctx, mac, ViewerHistoryLimit)
	if err != nil {
		s.log.Warn("doorhistory list failed", "mac", mac, "err", err)
		return nil, 0
	}
	unread, err := s.history.UnreadCount(ctx, mac)
	if err != nil {
		s.log.Warn("doorhistory unread count failed", "mac", mac, "err", err)
		unread = 0
	}
	return list, unread
}

// formatGermanDate renders "Di, 13. Mai" style strings for the
// clock-area in the intercom topbar.
func formatGermanDate(t time.Time) string {
	weekdays := [...]string{"So", "Mo", "Di", "Mi", "Do", "Fr", "Sa"}
	months := [...]string{
		"Januar", "Februar", "Maerz", "April", "Mai", "Juni",
		"Juli", "August", "September", "Oktober", "November", "Dezember",
	}
	t = t.Local()
	day := t.Day()
	month := months[int(t.Month())-1]
	return weekdays[int(t.Weekday())] + ", " +
		formatInt(day) + ". " + month
}

// formatGermanWhen renders "Heute 23:36" / "Gestern 19:14" /
// "Mi, 11.5. 14:02" for the history-sheet rows.
func formatGermanWhen(t time.Time) string {
	t = t.Local()
	now := time.Now()
	hhmm := t.Format("15:04")
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if !t.Before(startOfToday) {
		return "Heute " + hhmm
	}
	if !t.Before(startOfToday.AddDate(0, 0, -1)) {
		return "Gestern " + hhmm
	}
	weekdays := [...]string{"So", "Mo", "Di", "Mi", "Do", "Fr", "Sa"}
	return weekdays[int(t.Weekday())] + ", " +
		formatInt(t.Day()) + "." + formatInt(int(t.Month())) + ". " + hhmm
}

// formatInt is a small int-to-string helper that avoids pulling
// strconv into this file just for two call-sites.
func formatInt(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [11]byte
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

// safePrefix returns the first 8 chars of a MAC for logging
// without leaking the full address.
func safePrefix(mac string) string {
	if len(mac) < 8 {
		return mac
	}
	return mac[:8]
}

// fetchHomeWeather returns the cached open-meteo snapshot for the
// configured station coordinates, or nil if either no weather
// client is wired or the backend is unreachable. The template
// hides its weather block on nil so a degraded screensaver still
// shows clock + date.
func (s *Server) fetchHomeWeather(r *http.Request) *weather.Snapshot {
	if s.weather == nil {
		return nil
	}
	lat, lon := s.stationCoords(r)
	snap, err := s.weather.Get(r.Context(), lat, lon)
	if err != nil {
		return nil
	}
	return &snap
}
