package httpserver

import (
	"context"
	"errors"
	"net/http"
	"time"

	"carvilon.local/server/internal/doorhistory"
	"carvilon.local/server/internal/viewermanager"
	"carvilon.local/server/internal/weather"
)

// ViewerHistoryLimit caps the number of door_events shown on the
// /m/ list.
const ViewerHistoryLimit = 20

// viewerHomeData is the payload for the tenant home template.
// The flat structure mirrors the template field names so renames
// stay in lock-step.
//
// StandbyDoorID is intentionally absent; the standby-unlock JS
// posts to the literal /webviewer/doors/standby/unlock route and
// the server reads viewer.paired_intercom_mac.
type viewerHomeData struct {
	UnitName     string
	DoorName     string
	Now          string // "HH:MM:SS"
	NowDate      string // "Di, 13. Mai"
	DND          bool
	HasUnread    bool
	UnreadCount  int                // numeric count for the history-button badge
	HistoryItems []viewerHistoryRow // {Where, When, Unread}
	// Idle-view fields.
	IdleViewMode string            // "screensaver" or "livestream"
	Weather      *weather.Snapshot // nil = backend unreachable, hide weather block
	// Inline-mode payload. AutoScreensaverSeconds is the
	// persisted timer (0 = disabled); the browser runtime
	// promotes the setting into the slide-up modes container.
	AutoScreensaverSeconds int
	// Hydrates the inline settings-mode "Verlauf-Erfassung"
	// radio group. True = capture active.
	HistoryCaptureEnabled bool
	// clock-layout. Initial paint of the screensaver +
	// settings radio.
	ClockLayout string
}

// viewerHistoryRow is one row in the history-sheet template.
type viewerHistoryRow struct {
	Where  string
	When   string
	Unread bool
}

// handleHome renders the tenant intercom-viewer page. The
// template produces the markup; we provide the data.
//
// Mark-read variant A is active: after rendering, the displayed
// unread events are asynchronously marked as read.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	mac := ViewerMACFromContext(r.Context())
	if mac == "" {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	info, err := s.viewerMgr.GetViewerInfo(r.Context(), mac)
	if err != nil {
		if errors.Is(err, viewermanager.ErrViewerNotFound) {
			s.clearSessionCookie(w)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		s.log.Error("get viewer info", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	history, unread := s.loadViewerHistory(r.Context(), mac)
	// Resolve every row's intercom MAC to a human door name via
	// ONE UA-API round-trip per render. loadDoorMeta returns the
	// full door list too so rows without an intercom MAC
	// (door_unlocked events) can fall back to the single
	// existing door's name when the installation has only one
	// door.
	meta := s.loadDoorMeta(r.Context())
	rows := make([]viewerHistoryRow, 0, len(history))
	displayedIDs := make([]int64, 0, len(history))
	for _, ev := range history {
		rows = append(rows, viewerHistoryRow{
			Where:  resolveDoorName(meta, ev.IntercomMAC),
			When:   formatGermanWhen(ev.OccurredAt),
			Unread: ev.ReadAt == nil,
		})
		if ev.ReadAt == nil {
			displayedIDs = append(displayedIDs, ev.ID)
		}
	}

	now := time.Now()
	// The cam-label door name flows through the same resolver as
	// the history rows so a UA-Console rename is reflected in one
	// render cycle. PairedIntercomMAC is the natural lookup key;
	// single-door installs fall through to that one door
	// regardless.
	camDoorName := resolveDoorName(meta, info.PairedIntercomMAC)
	data := viewerHomeData{
		UnitName:               info.Name,
		DoorName:               camDoorName,
		Now:                    now.Format("15:04:05"),
		NowDate:                formatGermanDate(now),
		DND:                    false,
		HasUnread:              unread > 0,
		UnreadCount:            unread,
		HistoryItems:           rows,
		IdleViewMode:           info.ResolveIdleViewMode(),
		Weather:                s.fetchHomeWeather(r),
		AutoScreensaverSeconds: info.ResolveAutoScreensaverSeconds(),
		HistoryCaptureEnabled:  info.ResolveHistoryCaptureEnabled(),
		ClockLayout:            info.ResolveClockLayout(),
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
//
// The language is resolved from the calling tenant (session or
// bearer context). The shared resolveTenantLanguage helper falls
// back to German if no MAC is on the context, which keeps the
// admin /a/weather behaviour intact.
func (s *Server) fetchHomeWeather(r *http.Request) *weather.Snapshot {
	if s.weather == nil {
		return nil
	}
	lat, lon := s.stationCoords(r)
	lang := s.resolveTenantLanguage(r.Context())
	snap, err := s.weather.Get(r.Context(), lat, lon, lang)
	if err != nil {
		return nil
	}
	return &snap
}
