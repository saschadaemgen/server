// Saison 14-03: JSON endpoint behind the inline-history mode of
// the home page. Returns the most-recent doorbell events for the
// authenticated viewer and asynchronously marks the unread rows
// as read - the client already received them once via this
// payload, so subsequent opens see them as read.
//
// Saison 14-04-Phase2 extends the endpoint with three things:
//
//  1. Pagination via ?offset=...&limit=... (limit clamped to 50)
//  2. Date-filter via ?from=YYYY-MM-DD&to=YYYY-MM-DD (inclusive)
//  3. Mieter-soft-delete: only NON-hidden rows reach the
//     payload, and the response carries capture_enabled so the
//     UI can show "Erfassung deaktiviert"-Hinweis instead of an
//     empty list.
//
// Route: GET /webviewer/history.json (requireSession)
// Auth:  Mieter-Session-Cookie. MAC comes from the context.
// Body:  see mieterHistoryResponse.
package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"carvilon.local/server/internal/doorhistory"
	"carvilon.local/server/internal/mockmanager"
)

// mieterHistoryResponse is the JSON envelope. We always emit the
// "events" key (never nil) so the client can render an empty list
// without a null-check.
//
// Saison 14-04-Phase2 fields:
//   - HasMore signals whether another page exists beyond this one
//     (offset + len(events) < total).
//   - NextOffset is the offset the client should send to fetch the
//     next page. Zero when HasMore is false.
//   - CaptureEnabled mirrors the viewer's history_capture_enabled
//     toggle; the client renders a "Erfassung deaktiviert"-Hinweis
//     when false and skips rendering the (empty) events list.
type mieterHistoryResponse struct {
	Events         []mieterHistoryItem `json:"events"`
	HasMore        bool                `json:"has_more"`
	NextOffset     int                 `json:"next_offset"`
	CaptureEnabled bool                `json:"capture_enabled"`
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

// mieterHistoryDefaultLimit is the page size when no ?limit= is
// passed. ListOptsMaxLimit (50) caps the upper bound. Both align
// with the briefing's "20 default, 50 max"-spec.
const mieterHistoryDefaultLimit = 20

// mieterHistoryMaxOffset is a Sanity-Bound: kein Client darf den
// Server zwingen 10001+ Rows zu skippen. Briefing 4.1 verlangt
// 0..10000.
const mieterHistoryMaxOffset = 10000

// mieterHistoryDateLayout entspricht dem HTML <input type="date">
// Standard. Anderer Format -> 400.
const mieterHistoryDateLayout = "2006-01-02"

func (s *Server) handleMieterHistoryJSON(w http.ResponseWriter, r *http.Request) {
	mac := ViewerMACFromContext(r.Context())
	if mac == "" {
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}

	opts, err := parseHistoryListOpts(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Capture-Toggle: wenn der Mieter die Erfassung deaktiviert
	// hat, liefern wir eine leere Liste mit capture_enabled=false.
	// Pagination + Mark-Read entfaellt - es gibt nichts anzuzeigen.
	captureEnabled := true
	info, infoErr := s.mockMgr.GetViewerInfo(r.Context(), mac)
	if infoErr == nil {
		captureEnabled = info.ResolveHistoryCaptureEnabled()
	} else if !errors.Is(infoErr, mockmanager.ErrViewerNotFound) {
		s.log.Warn("history viewer info failed",
			"mac_prefix", safePrefix(mac), "err", infoErr)
	}

	if !captureEnabled {
		writeMieterHistoryJSON(w, mieterHistoryResponse{
			Events:         []mieterHistoryItem{},
			HasMore:        false,
			NextOffset:     0,
			CaptureEnabled: false,
		})
		return
	}

	if s.history == nil {
		writeMieterHistoryJSON(w, mieterHistoryResponse{
			Events:         []mieterHistoryItem{},
			CaptureEnabled: true,
		})
		return
	}

	events, err := s.history.ListVisible(r.Context(), mac, opts)
	if err != nil {
		s.log.Warn("doorhistory list visible failed",
			"mac_prefix", safePrefix(mac), "err", err)
		http.Error(w, "history list failed", http.StatusInternalServerError)
		return
	}
	totalVisible, err := s.history.CountVisible(r.Context(), mac, opts)
	if err != nil {
		s.log.Warn("doorhistory count visible failed",
			"mac_prefix", safePrefix(mac), "err", err)
		// Soft-fail: HasMore wird falsch sein, aber wir liefern die
		// Seite trotzdem aus.
		totalVisible = opts.Offset + len(events)
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

	hasMore := opts.Offset+len(items) < totalVisible
	nextOffset := 0
	if hasMore {
		nextOffset = opts.Offset + len(items)
	}
	writeMieterHistoryJSON(w, mieterHistoryResponse{
		Events:         items,
		HasMore:        hasMore,
		NextOffset:     nextOffset,
		CaptureEnabled: true,
	})

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

func writeMieterHistoryJSON(w http.ResponseWriter, resp mieterHistoryResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}

// parseHistoryListOpts validates the four query parameters and
// translates them into a doorhistory.ListOpts. Invalid input
// returns a deutsche-Fehlermeldung; the handler maps that to 400.
//
// Saison 14-04-Phase2.
func parseHistoryListOpts(r *http.Request) (doorhistory.ListOpts, error) {
	opts := doorhistory.ListOpts{Limit: mieterHistoryDefaultLimit}

	q := r.URL.Query()
	if raw := strings.TrimSpace(q.Get("offset")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 || v > mieterHistoryMaxOffset {
			return doorhistory.ListOpts{}, errors.New("offset muss zwischen 0 und 10000 liegen")
		}
		opts.Offset = v
	}
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 || v > doorhistory.ListOptsMaxLimit {
			return doorhistory.ListOpts{}, errors.New("limit muss zwischen 1 und 50 liegen")
		}
		opts.Limit = v
	}
	if raw := strings.TrimSpace(q.Get("from")); raw != "" {
		t, err := time.Parse(mieterHistoryDateLayout, raw)
		if err != nil {
			return doorhistory.ListOpts{}, errors.New("from muss YYYY-MM-DD sein")
		}
		opts.From = t
	}
	if raw := strings.TrimSpace(q.Get("to")); raw != "" {
		t, err := time.Parse(mieterHistoryDateLayout, raw)
		if err != nil {
			return doorhistory.ListOpts{}, errors.New("to muss YYYY-MM-DD sein")
		}
		opts.To = t
	}
	if !opts.From.IsZero() && !opts.To.IsZero() && opts.To.Before(opts.From) {
		return doorhistory.ListOpts{}, errors.New("to liegt vor from")
	}
	return opts, nil
}
