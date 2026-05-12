package httpserver

import (
	"context"
	"errors"
	"net/http"
	"time"

	"unifix.local/server/internal/doorhistory"
	"unifix.local/server/internal/mockmanager"
)

// MieterHistoryLimit caps the number of door_events shown on the
// /m/ list. Twenty is generous for a single household; older
// entries are still in the DB and surface through an admin
// detail page (Saison 13-02 or later).
const MieterHistoryLimit = 20

type mieterHomeData struct {
	MockMAC     string
	MockName    string
	History     []mieterHistoryRow
	UnreadCount int
}

// mieterHistoryRow is the per-row payload for the history card.
// We pre-format strings server-side so the template stays small
// and de-CH locale conventions stay in one place.
type mieterHistoryRow struct {
	ID          int64
	OccurredAt  string
	Status      string
	IntercomMAC string
	Unread      bool
}

// handleHome renders the tenant landing page. The page hosts an
// EventSource subscription on /m/events, a hidden doorbell
// overlay, and a list of the most recent doorbell events.
//
// Saison 13-01: every render also marks the displayed events as
// read (Variante A from the briefing). Live pushes via SSE that
// arrive after the render stay unread until the next reload, so
// the user can see "neu" appear when they leave the page open.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	mac := MockMACFromContext(r.Context())
	info, err := s.mockMgr.GetViewerInfo(r.Context(), mac)
	if err != nil {
		if errors.Is(err, mockmanager.ErrViewerNotFound) {
			http.Redirect(w, r, "/m/login", http.StatusSeeOther)
			return
		}
		s.log.Error("get viewer info", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	history, unread := s.loadMieterHistory(r.Context(), mac)
	rows := make([]mieterHistoryRow, 0, len(history))
	displayedIDs := make([]int64, 0, len(history))
	for _, ev := range history {
		rows = append(rows, mieterHistoryRow{
			ID:          ev.ID,
			OccurredAt:  ev.OccurredAt.Local().Format("02.01.2006 15:04"),
			Status:      statusFor(ev),
			IntercomMAC: ev.IntercomMAC,
			Unread:      ev.ReadAt == nil,
		})
		if ev.ReadAt == nil {
			displayedIDs = append(displayedIDs, ev.ID)
		}
	}

	data := mieterHomeData{
		MockMAC:     info.MAC,
		MockName:    info.Name,
		History:     rows,
		UnreadCount: unread,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.renderMieter(w, "home", data); err != nil {
		s.log.Error("render mieter home", "err", err)
		return
	}

	// Variante A: mark the rendered (and previously unread) events
	// as read AFTER the page is flushed. A failure here is benign;
	// the next page load corrects it. We use a fresh context so a
	// client disconnect during write does not cancel the update.
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

// loadMieterHistory returns the list plus unread count, both 0 if
// the history store is not wired (tests, narrow setups).
func (s *Server) loadMieterHistory(ctx context.Context, mac string) ([]doorhistory.Event, int) {
	if s.history == nil {
		return nil, 0
	}
	list, err := s.history.ListForMock(ctx, mac, MieterHistoryLimit)
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

// statusFor maps the four time fields on a door_events row to a
// short German status label for the mieter UI. Most rows are
// either "laeuft" (in flight) or "abgebrochen"; "beantwortet" and
// "beendet" are wired now even though answer / end events get
// written first in Saison 13-03.
func statusFor(ev doorhistory.Event) string {
	switch {
	case ev.EndedAt != nil:
		return "beendet"
	case ev.AnsweredAt != nil:
		return "beantwortet"
	case ev.CancelledAt != nil:
		return "abgebrochen"
	default:
		return "laeuft"
	}
}

// safePrefix returns the first 8 chars of a MAC for logging
// without leaking the full address. Falls back to the whole
// string for unexpectedly short input.
func safePrefix(mac string) string {
	if len(mac) < 8 {
		return mac
	}
	return mac[:8]
}
