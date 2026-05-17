// Saison 14-03: JSON endpoint behind the inline-history mode of
// the home page. Returns the most-recent doorbell events for the
// authenticated viewer and asynchronously marks the unread rows
// as read - the client already received them once via this
// payload, so subsequent opens see them as read.
//
// Variante A (mark-read after render) mirrors the server-rendered
// /webviewer page in handler_home.go; the inline-history mode just
// performs the equivalent over JSON for the slide-up view.
//
// Route: GET /webviewer/history.json (requireSession)
// Auth:  Mieter-Session-Cookie. MAC comes from the context.
// Body:  {"events": [{...}]}, with at most ViewerHistoryLimit rows.
package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// mieterHistoryResponse is the JSON envelope. We always emit the
// "events" key (never nil) so the client can render an empty list
// without a null-check.
type mieterHistoryResponse struct {
	Events []mieterHistoryItem `json:"events"`
}

// mieterHistoryItem is one row in the response. The shape is
// kept narrow on purpose: the UI only needs enough to display
// "Wo, wann, NEU" rows. door_name falls back to the intercom MAC
// when no UA-side lookup has resolved a friendly name; the
// briefing explicitly allows this fallback to keep the endpoint
// cheap.
type mieterHistoryItem struct {
	ID          int64  `json:"id"`
	CreatedAt   int64  `json:"created_at"`
	When        string `json:"when"`
	DoorName    string `json:"door_name,omitempty"`
	IntercomMAC string `json:"intercom_mac,omitempty"`
	EventType   string `json:"event_type"`
	Unread      bool   `json:"unread"`
}

func (s *Server) handleMieterHistoryJSON(w http.ResponseWriter, r *http.Request) {
	mac := ViewerMACFromContext(r.Context())
	if mac == "" {
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}
	if s.history == nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(mieterHistoryResponse{Events: []mieterHistoryItem{}})
		return
	}

	events, err := s.history.ListForMock(r.Context(), mac, ViewerHistoryLimit)
	if err != nil {
		s.log.Warn("doorhistory list json failed", "mac_prefix", safePrefix(mac), "err", err)
		http.Error(w, "history list failed", http.StatusInternalServerError)
		return
	}

	// Saison 14-03-FIX02/FIX03: ONE ListDoors call per render,
	// then resolve every row through the cached doorMeta. The
	// single-door fallback inside resolveDoorName covers the
	// door_unlocked event-type which often has no intercom MAC.
	meta := s.loadDoorMeta(r.Context())

	items := make([]mieterHistoryItem, 0, len(events))
	unreadIDs := make([]int64, 0, len(events))
	for _, ev := range events {
		items = append(items, mieterHistoryItem{
			ID:          ev.ID,
			CreatedAt:   ev.OccurredAt.Unix(),
			When:        formatGermanWhen(ev.OccurredAt),
			IntercomMAC: ev.IntercomMAC,
			DoorName:    resolveDoorName(meta, ev.IntercomMAC),
			EventType:   ev.EventType,
			Unread:      ev.ReadAt == nil,
		})
		if ev.ReadAt == nil {
			unreadIDs = append(unreadIDs, ev.ID)
		}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(mieterHistoryResponse{Events: items})

	if len(unreadIDs) > 0 {
		go func(ids []int64) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.history.MarkRead(ctx, mac, ids); err != nil {
				s.log.Warn("doorhistory mark read (json) failed",
					"mac_prefix", safePrefix(mac), "err", err)
				return
			}
			// Saison 14-03-FIX03 Sub-2: tell every subscriber on
			// this mock that the count just dropped (usually to
			// 0). The screensaver badge uses this to hide itself
			// without polling /webviewer/unread-count.
			if s.hub != nil {
				s.hub.BroadcastUnreadCount(ctx, mac)
			}
		}(unreadIDs)
	}
}
