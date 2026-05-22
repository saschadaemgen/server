// JSON endpoint behind the inline-history mode of the home page.
// Returns the most-recent doorbell events for the authenticated
// viewer and asynchronously marks the unread rows as read - the
// client already received them once via this payload, so
// subsequent opens see them as read.
//
// The endpoint supports:
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
//
// The actual list/hide logic lives in MAC-parametrised
// serveHistory* helpers so the ESP-Bearer-tree (/esp/history*)
// can reuse them. The Mieter wrappers below just pull the MAC
// from the session context and delegate; the ESP wrappers in
// handler_esp_history.go pull from the bearer context. Shared
// validation lives in parseHistoryListOpts so both surfaces
// reject identical bogus offset/limit/from/to inputs.
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
	"carvilon.local/server/internal/viewermanager"
)

// mieterHistoryResponse is the JSON envelope. We always emit the
// "events" key (never nil) so the client can render an empty list
// without a null-check.
//
// Pagination + capture fields:
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

// mieterHistoryMaxOffset is a sanity bound: no client gets to
// force the server to skip 10001+ rows.
const mieterHistoryMaxOffset = 10000

// mieterHistoryDateLayout matches the HTML <input type="date">
// default. Any other format -> 400.
const mieterHistoryDateLayout = "2006-01-02"

func (s *Server) handleMieterHistoryJSON(w http.ResponseWriter, r *http.Request) {
	mac := ViewerMACFromContext(r.Context())
	if mac == "" {
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}
	s.serveHistoryList(w, r, mac)
}

// serveHistoryList implements the GET history list flow shared by
// the Mieter-Session-Cookie and the ESP-Bearer-Token surfaces. The
// MAC argument is whatever the caller's auth middleware verified;
// the helper itself does no auth.
//
// Validation, capture-toggle, pagination, mark-read fan-out and
// the response shape are identical across both surfaces - the only
// thing that differs is the auth gate.
func (s *Server) serveHistoryList(w http.ResponseWriter, r *http.Request, mac string) {
	opts, err := parseHistoryListOpts(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if opts.Limit == 0 {
		opts.Limit = mieterHistoryDefaultLimit
	}

	// Capture toggle: when the mieter has disabled history capture
	// we return an empty list with capture_enabled=false.
	// Pagination + mark-read are skipped - there is nothing to show.
	captureEnabled := true
	info, infoErr := s.viewerMgr.GetViewerInfo(r.Context(), mac)
	if infoErr == nil {
		captureEnabled = info.ResolveHistoryCaptureEnabled()
	} else if !errors.Is(infoErr, viewermanager.ErrViewerNotFound) {
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
		// Soft-fail: HasMore will be wrong but we still serve the
		// page.
		totalVisible = opts.Offset + len(events)
	}

	// ONE ListDoors call per render, then resolve every row
	// through the cached doorMeta. The single-door fallback inside
	// resolveDoorName covers the door_unlocked event-type which
	// often has no intercom MAC.
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
			// Tell every subscriber on this mock that the count
			// just dropped (usually to 0). The screensaver badge
			// uses this to hide itself without polling
			// /webviewer/unread-count.
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

// handleMieterHistoryHideOne soft-deletes a single door_events
// row for the calling viewer. event_id stems from the path; the
// doorhistory layer enforces the mock-scope so a stray id from
// another viewer returns 404.
//
// Route: DELETE /webviewer/history/{event_id}
func (s *Server) handleMieterHistoryHideOne(w http.ResponseWriter, r *http.Request) {
	mac := ViewerMACFromContext(r.Context())
	if mac == "" {
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}
	s.serveHistoryHideOne(w, r, mac)
}

// serveHistoryHideOne soft-hides a single event for the given mac.
// Shared between the Mieter (cookie-auth) and ESP (bearer-auth)
// surfaces; the event_id arrives via PathValue("event_id"), which
// both surfaces register with the same wildcard name. After a
// successful hide we re-broadcast the unread-count so the
// screensaver badge tracks the change live on every connected
// device (web + esp) instead of polling.
func (s *Server) serveHistoryHideOne(w http.ResponseWriter, r *http.Request, mac string) {
	if s.history == nil {
		http.Error(w, "history not configured", http.StatusServiceUnavailable)
		return
	}
	idRaw := r.PathValue("event_id")
	id, err := strconv.ParseInt(idRaw, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "event_id muss eine positive Zahl sein", http.StatusBadRequest)
		return
	}
	if err := s.history.HideEvent(r.Context(), mac, id); err != nil {
		if errors.Is(err, doorhistory.ErrNotFound) {
			http.Error(w, "Eintrag nicht gefunden.", http.StatusNotFound)
			return
		}
		s.log.Error("hide event", "err", err, "mac_prefix", safePrefix(mac), "event_id", id)
		http.Error(w, "Verstecken fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	// The unread count may have just dropped if the hidden entry
	// still had read_at IS NULL. The broadcast lets all tabs +
	// the ESP hardware pull the badge in sync.
	if s.hub != nil {
		s.hub.BroadcastUnreadCount(r.Context(), mac)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":     true,
		"hidden": true,
	})
}

// handleMieterHistoryHideAll soft-deletes every currently-visible
// row for the caller. Idempotent: a second call right after this
// one returns hidden_count=0. We do NOT trigger config.changed -
// the change is purely visible-history-state, not config; a
// follow-up SSE unread-count broadcast inside the doorbellhub
// layer is left for a later iteration; today's mieter UI reload
// after "Verlauf leeren" is enough.
//
// Route: DELETE /webviewer/history
func (s *Server) handleMieterHistoryHideAll(w http.ResponseWriter, r *http.Request) {
	mac := ViewerMACFromContext(r.Context())
	if mac == "" {
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}
	s.serveHistoryHideAll(w, r, mac)
}

// serveHistoryHideAll soft-hides every currently-visible event
// for the given mac. Shared between the Mieter and ESP surfaces.
// Idempotent: a second call returns hidden_count=0.
func (s *Server) serveHistoryHideAll(w http.ResponseWriter, r *http.Request, mac string) {
	if s.history == nil {
		http.Error(w, "history not configured", http.StatusServiceUnavailable)
		return
	}
	n, err := s.history.HideAllEvents(r.Context(), mac)
	if err != nil {
		s.log.Error("hide all events", "err", err, "mac_prefix", safePrefix(mac))
		http.Error(w, "Loeschen fehlgeschlagen.", http.StatusInternalServerError)
		return
	}
	// Unread-count broadcast: after HideAll there are no visible
	// unread rows left. The browser can drop the badge to 0
	// immediately without polling again.
	if s.hub != nil {
		s.hub.BroadcastUnreadCount(r.Context(), mac)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":           true,
		"hidden_count": n,
	})
}

// parseHistoryListOpts validates the four query parameters and
// translates them into a doorhistory.ListOpts. Invalid input
// returns a German error message; the handler maps that to 400.
//
// When the client does not send ?limit=, opts.Limit stays at 0;
// every handler sets its own page default (mieter: 20, admin:
// 50) before calling ListVisible/AdminListAll. That way callers
// with identical parser logic can serve different defaults
// without the parser having to know about them.
func parseHistoryListOpts(r *http.Request) (doorhistory.ListOpts, error) {
	var opts doorhistory.ListOpts

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
